package mtproto

import (
	"crypto/sha256"
	"fmt"
	"math/rand"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"time"

	"github.com/ansel1/merry"
)

//go:generate go run scheme/generate_tl_schema.go 75 scheme/tl-schema-75.tl tl_schema.go
//go:generate gofmt -w tl_schema.go

const ROUTINES_COUNT = 4

var ErrNoSessionData = merry.New("no session data")

type SessionInfo struct {
	DcID        int32
	AuthKey     []byte
	AuthKeyHash []byte
	ServerSalt  int64
	Addr        string
	sessionId   int64
}

type SessionStore interface {
	Save(*SessionInfo) error
	Load(*SessionInfo) error
}

type SessNoopStore struct{}

func (s *SessNoopStore) Save(sess *SessionInfo) error { return nil }
func (s *SessNoopStore) Load(sess *SessionInfo) error { return merry.New("can not load") }

type SessFileStore struct {
	FPath string
}

func (s *SessFileStore) Save(sess *SessionInfo) (err error) {
	f, err := os.Create(s.FPath)
	if err != nil {
		return merry.Wrap(err)
	}
	defer f.Close()

	b := NewEncodeBuf(1024)
	b.StringBytes(sess.AuthKey)
	b.StringBytes(sess.AuthKeyHash)
	b.Long(sess.ServerSalt)
	b.String(sess.Addr)

	_, err = f.Write(b.buf)
	if err != nil {
		return merry.Wrap(err)
	}
	return nil
}

func (s *SessFileStore) Load(sess *SessionInfo) error {
	f, err := os.Open(s.FPath)
	if os.IsNotExist(err) {
		return ErrNoSessionData.Here()
	}
	if err != nil {
		return merry.Wrap(err)
	}
	defer f.Close()

	b := make([]byte, 1024*4)
	_, err = f.Read(b)
	if err != nil {
		return merry.Wrap(err)
	}

	d := NewDecodeBuf(b)
	sess.AuthKey = d.StringBytes()
	sess.AuthKeyHash = d.StringBytes()
	sess.ServerSalt = d.Long()
	sess.Addr = d.String()

	if d.err != nil {
		return merry.Wrap(d.err)
	}
	return nil
}

type AppConfig struct {
	AppID          int32
	AppHash        string
	AppVersion     string
	DeviceModel    string
	SystemVersion  string
	SystemLangCode string
	LangPack       string
	LangCode       string
}

type MTProto struct {
	sessionStore SessionStore
	session      *SessionInfo
	appCfg       *AppConfig
	conn         *net.TCPConn
	log          Logger

	// Two queues here.
	// First (external) has limited size and contains external requests.
	// Second (internal) is unlimited. Special goroutine transfers messages
	// from external to internal queue while len(interbal) < cap(external).
	// This allows throttling (as same as single limited queue).
	// And this allow to safely (without locks) return any failed (due to
	// network probles for example) messages back to internal queue and retry later.
	extSendQueue chan *packetToSend //external
	sendQueue    chan *packetToSend //internal

	routinesStop chan struct{}
	routinesWG   sync.WaitGroup

	mutex           *sync.Mutex
	reconnSemaphore chan struct{}
	encryptionReady bool
	lastSeqNo       int32
	msgsByID        map[int64]*packetToSend
	seqNo           int32
	msgId           int64
	handleEvent     func(TL)

	dcOptions []*TL_dcOption
}

type packetToSend struct {
	msgID   int64
	seqNo   int32
	msg     TL
	resp    chan TL
	needAck bool
}

func newPacket(msg TL, resp chan TL) *packetToSend {
	return &packetToSend{msg: msg, resp: resp}
}

func NewMTProto(appID int32, appHash string) *MTProto {
	log := &SimpleLogHandler{}

	// getting exec directory
	var exPath string
	ex, err := os.Executable()
	if err != nil {
		Logger{log}.Error(err, "failed to get executable file path")
		exPath = "."
	} else {
		exPath = filepath.Dir(ex)
	}

	cfg := &AppConfig{
		AppID:          appID,
		AppHash:        appHash,
		AppVersion:     "0.0.1",
		DeviceModel:    "Unknown",
		SystemVersion:  runtime.GOOS + "/" + runtime.GOARCH,
		SystemLangCode: "en",
		LangPack:       "",
		LangCode:       "en",
	}
	return NewMTProtoExt(cfg, &SessFileStore{exPath + "/tg.session"}, log, nil)
}

func NewMTProtoExt(appCfg *AppConfig, sessStore SessionStore, logHandler LogHandler, session *SessionInfo) *MTProto {
	m := &MTProto{
		sessionStore: sessStore,
		session:      session,
		appCfg:       appCfg,
		log:          Logger{logHandler},

		extSendQueue: make(chan *packetToSend, 64),
		sendQueue:    make(chan *packetToSend, 1024),
		routinesStop: make(chan struct{}, ROUTINES_COUNT),

		msgsByID:        make(map[int64]*packetToSend),
		mutex:           &sync.Mutex{},
		reconnSemaphore: make(chan struct{}, 1),
	}
	go m.debugRoutine()
	return m
}

func (m *MTProto) InitSessAndConnect() error {
	if err := m.InitSession(false); err != nil {
		return merry.Wrap(err)
	}
	if err := m.Connect(); err != nil {
		return merry.Wrap(err)
	}
	return nil
}

func (m *MTProto) InitSession(sessEncrIsReady bool) error {
	if m.session == nil {
		m.session = &SessionInfo{}
		err := m.sessionStore.Load(m.session)
		if merry.Is(err, ErrNoSessionData) { //no data
			m.session.Addr = "149.154.167.50:443" //"149.154.167.40"
			m.encryptionReady = false
		} else if err == nil { //got saved session
			m.encryptionReady = true
		} else {
			return merry.Wrap(err)
		}
	} else {
		m.encryptionReady = sessEncrIsReady
	}

	rand.Seed(time.Now().UnixNano())
	m.session.sessionId = rand.Int63()
	return nil
}

func (m *MTProto) AppConfig() *AppConfig {
	return m.appCfg
}

func (m *MTProto) LogHandler() LogHandler {
	return m.log.hnd
}

func (m *MTProto) CopySession() *SessionInfo {
	sess := *m.session
	return &sess
}

func (m *MTProto) SaveSessionLogged() {
	if err := m.sessionStore.Save(m.session); err != nil {
		m.log.Error(err, "failed to save session data")
	}
}

func (m *MTProto) DCAddr(dcID int32, ipv6 bool) (string, bool) {
	for _, o := range m.dcOptions {
		if o.ID == dcID && o.Ipv6 == ipv6 {
			return fmt.Sprintf("%s:%d", o.IpAddress, o.Port), true
		}
	}
	return "", false
}

func (m *MTProto) SetEventsHandler(handler func(TL)) {
	m.handleEvent = handler
}

func (m *MTProto) Connect() error {
	m.log.Info("connecting to DC %d (%s)...", m.session.DcID, m.session.Addr)
	tcpAddr, err := net.ResolveTCPAddr("tcp", m.session.Addr)
	if err != nil {
		return merry.Wrap(err)
	}
	m.conn, err = net.DialTCP("tcp", nil, tcpAddr)
	if err != nil {
		return merry.Wrap(err)
	}
	_, err = m.conn.Write([]byte{0xef})
	if err != nil {
		return merry.Wrap(err)
	}

	// getting new authKey if need
	if !m.encryptionReady {
		if err = m.makeAuthKey(); err != nil {
			return merry.Wrap(err)
		}
		if err := m.sessionStore.Save(m.session); err != nil {
			return merry.Wrap(err)
		}
		m.encryptionReady = true
	}

	// starting goroutines
	m.log.Debug("connecting: starting routines...")
	m.routinesWG.Add(2)
	go m.sendRoutine()
	go m.readRoutine()

	// getting connection configs
	m.log.Debug("connecting: getting config...")
	x := m.sendSyncInternal(TL_invokeWithLayer{
		TL_Layer,
		TL_initConnection{
			m.appCfg.AppID,
			m.appCfg.DeviceModel,
			m.appCfg.SystemVersion,
			m.appCfg.AppVersion,
			m.appCfg.SystemLangCode,
			m.appCfg.LangPack,
			m.appCfg.LangCode,
			TL_help_getConfig{},
		},
	})
	if cfg, ok := x.(TL_config); ok {
		m.session.DcID = cfg.ThisDc
		for _, v := range cfg.DcOptions {
			v := v.(TL_dcOption)
			m.dcOptions = append(m.dcOptions, &v)
		}
	} else {
		return WrongRespError(x)
	}

	m.routinesWG.Add(2)
	go m.queueTransferRoutine() // straintg messages transfer from external to internal queue
	go m.pingRoutine()          // starting keepalive pinging
	m.log.Info("connected to DC %d (%s)...", m.session.DcID, m.session.Addr)
	return nil
}

func (m *MTProto) reconnectLogged() {
	m.log.Info("reconnecting...")
	select {
	case m.reconnSemaphore <- struct{}{}:
	default:
		m.log.Info("reconnection already in progress, aborting")
		return
	}
	defer func() { <-m.reconnSemaphore }()

	for {
		err := m.reconnectToDc(m.session.DcID)
		if err == nil {
			return
		}
		m.log.Error(err, "failed to reconnect")
		m.log.Info("retrying in 5 seconds")
		time.Sleep(5 * time.Second)
		// and trying to reconnect again
	}
}

func (m *MTProto) Reconnect() error {
	return m.reconnectToDc(m.session.DcID)
}

func (m *MTProto) reconnectToDc(newDcID int32) error {
	m.log.Info("reconnecting: DC %d -> %d", m.session.DcID, newDcID)

	// stopping routines
	m.log.Debug("stopping routines...")
	for i := 0; i < ROUTINES_COUNT; i++ {
		m.routinesStop <- struct{}{}
	}

	// closing connection, readRoutine will then fail to read() and will handle stop signal
	if m.conn != nil {
		if err := m.conn.Close(); err != nil && !IsClosedConnErr(err) {
			return merry.Wrap(err)
		}
	}

	// waiting for all routines to stop
	m.log.Debug("waiting for routines...")
	m.routinesWG.Wait()
	m.log.Debug("done stopping routines...")

	// removing unused stop signals (if any)
	for empty := false; !empty; {
		select {
		case <-m.routinesStop:
		default:
			empty = true
		}
	}

	// saving IDs of messages from msgsByID[],
	// some of them may not have been sent, so we'll resend them after reconnection
	pendingIDs := make([]int64, 0, len(m.msgsByID))
	for id := range m.msgsByID {
		pendingIDs = append(pendingIDs, id)
	}
	m.log.Debug("found %d pending packet(s)", len(pendingIDs))

	// renewing connection
	if newDcID != m.session.DcID {
		m.encryptionReady = false //TODO: export auth here (if authed)
		//https://github.com/sochix/TLSharp/blob/0940d3d982e9c22adac96b6c81a435403802899a/TLSharp.Core/TelegramClient.cs#L84
	}
	newDcAddr, ok := m.DCAddr(newDcID, false)
	if !ok {
		return merry.Errorf("wrong DC number: %d", newDcID)
	}
	m.session.DcID = newDcID
	m.session.Addr = newDcAddr
	if err := m.Connect(); err != nil {
		return merry.Wrap(err)
	}

	// Checking pending messages.
	// 1) some of them may have been answered, so they will not be in msgsByID[]
	// 2) some of them may have been received by TG, but response has not reached us yet
	// 3) some of them may be actually lost and must be resend
	// Here we resend messages both from (2) and (3). But since msgID and seqNo
	// are preserved, TG will ignore doubles from (2). And (3) will finally reach TG.
	if len(pendingIDs) > 0 {
		var packets []*packetToSend
		m.mutex.Lock()
		for _, id := range pendingIDs {
			packet, ok := m.msgsByID[id]
			if ok {
				packets = append(packets, packet)
			}
		}
		m.pushPendingPacketsUnlocked(packets)
		m.mutex.Unlock()
	}

	m.log.Info("reconnected to DC %d (%s)", m.session.DcID, m.session.Addr)
	return nil
}

func (m *MTProto) Send(msg TL) chan TL {
	resp := make(chan TL, 1)
	m.extSendQueue <- newPacket(msg, resp)
	return resp
}

func (m *MTProto) SendSync(msg TL) TL {
	resp := make(chan TL, 1)
	m.extSendQueue <- newPacket(msg, resp)
	return <-resp
}

func (m *MTProto) sendSyncInternal(msg TL) TL {
	resp := make(chan TL, 1)
	m.sendQueue <- newPacket(msg, resp)
	return <-resp
}

type AuthDataProvider interface {
	PhoneNumber() (string, error)
	Code() (string, error)
	Password() (string, error)
}

type ScanfAuthDataProvider struct{}

func (ap ScanfAuthDataProvider) PhoneNumber() (string, error) {
	var phonenumber string
	fmt.Print("Enter phone number: ")
	fmt.Scanf("%s", &phonenumber)
	return phonenumber, nil
}

func (ap ScanfAuthDataProvider) Code() (string, error) {
	var code string
	fmt.Print("Enter code: ")
	fmt.Scanf("%s", &code)
	return code, nil
}

func (ap ScanfAuthDataProvider) Password() (string, error) {
	var passwd string
	fmt.Print("Enter password: ")
	fmt.Scanf("%s", &passwd)
	return passwd, nil
}

func (m *MTProto) Auth(authData AuthDataProvider) error {
	phonenumber, err := authData.PhoneNumber()
	if err != nil {
		return merry.Wrap(err)
	}

	var authSentCode TL_auth_sentCode
	flag := true
	for flag {
		x := m.SendSync(TL_auth_sendCode{
			CurrentNumber: TL_boolTrue{},
			PhoneNumber:   phonenumber,
			ApiID:         m.appCfg.AppID,
			ApiHash:       m.appCfg.AppHash,
		})
		switch x.(type) {
		case TL_auth_sentCode:
			authSentCode = x.(TL_auth_sentCode)
			flag = false
		case TL_rpc_error:
			x := x.(TL_rpc_error)
			if x.ErrorCode != TL_ErrSeeOther {
				return WrongRespError(x)
			}
			var newDc int32
			n, _ := fmt.Sscanf(x.ErrorMessage, "PHONE_MIGRATE_%d", &newDc)
			if n != 1 {
				n, _ := fmt.Sscanf(x.ErrorMessage, "NETWORK_MIGRATE_%d", &newDc)
				if n != 1 {
					return fmt.Errorf("RPC error_string: %s", x.ErrorMessage)
				}
			}

			if err := m.reconnectToDc(newDc); err != nil {
				return merry.Wrap(err)
			}
			//TODO: save session here?
		default:
			return WrongRespError(x)
		}
	}

	code, err := authData.Code()
	if err != nil {
		return merry.Wrap(err)
	}

	//if authSentCode.Phone_registered
	x := m.SendSync(TL_auth_signIn{phonenumber, authSentCode.PhoneCodeHash, code})
	if IsError(x, "SESSION_PASSWORD_NEEDED") {
		x = m.SendSync(TL_account_getPassword{})
		accPasswd, ok := x.(TL_account_password)
		if !ok {
			return WrongRespError(x)
		}

		passwd, err := authData.Password()
		if err != nil {
			return merry.Wrap(err)
		}

		salt := string(accPasswd.CurrentSalt)
		hash := sha256.Sum256([]byte(salt + passwd + salt))
		x = m.SendSync(TL_auth_checkPassword{hash[:]})
		if _, ok := x.(TL_rpc_error); ok {
			return WrongRespError(x)
		}
	}
	auth, ok := x.(TL_auth_authorization)
	if !ok {
		return fmt.Errorf("RPC: %#v", x)
	}
	userSelf := auth.User.(TL_user)
	fmt.Printf("Signed in: id %d name <%s %s>\n", userSelf.ID, userSelf.FirstName, userSelf.LastName)
	return nil
}

func (m *MTProto) popPendingPacketsUnlocked() []*packetToSend {
	packets := make([]*packetToSend, 0, len(m.msgsByID))
	msgs := make([]TL, 0)
	for id, packet := range m.msgsByID {
		delete(m.msgsByID, id)
		packets = append(packets, packet)
		msgs = append(msgs, packet.msg)
	}
	m.log.Debug("popped %d pending packet(s): %#v", len(packets), msgs)
	return packets
}
func (m *MTProto) popPendingPackets() []*packetToSend {
	m.mutex.Lock()
	defer m.mutex.Unlock()
	return m.popPendingPacketsUnlocked()
}
func (m *MTProto) pushPendingPacketsUnlocked(packets []*packetToSend) {
	for _, packet := range packets {
		m.sendQueue <- packet
	}
	m.log.Debug("pushed %d pending packet(s)", len(packets))
}
func (m *MTProto) pushPendingPackets(packets []*packetToSend) {
	m.mutex.Lock()
	defer m.mutex.Unlock()
	m.pushPendingPacketsUnlocked(packets)
}
func (m *MTProto) resendPendingPackets() {
	m.mutex.Lock()
	defer m.mutex.Unlock()
	packets := m.popPendingPacketsUnlocked()
	m.pushPendingPacketsUnlocked(packets)
}

func (m *MTProto) GetContacts() error {
	x := m.SendSync(TL_contacts_getContacts{0})
	list, ok := x.(TL_contacts_contacts)
	if !ok {
		return fmt.Errorf("RPC: %#v", x)
	}

	contacts := make(map[int32]TL_user)
	for _, v := range list.Users {
		if v, ok := v.(TL_user); ok {
			contacts[v.ID] = v
		}
	}
	fmt.Printf(
		"\033[33m\033[1m%10s    %10s    %-30s    %-20s\033[0m\n",
		"id", "mutual", "name", "username",
	)
	for _, v := range list.Contacts {
		v := v.(TL_contact)
		fmt.Printf(
			"%10d    %10t    %-30s    %-20s\n",
			v.UserID,
			toBool(v.Mutual),
			fmt.Sprintf("%s %s", contacts[v.UserID].FirstName, contacts[v.UserID].LastName),
			contacts[v.UserID].Username,
		)
	}

	return nil
}

/*func (m *MTProto) SendMessage(user_id int32, msg string) error {
	resp := make(chan TL, 1)
	m.sendQueue <- packetToSend{
		TL_messages_sendMessage{
			TL_inputPeerContact{user_id},
			msg,
			rand.Int63(),
		},
		resp,
	}
	x := <-resp
	_, ok := x.(TL_messages_sentMessage)
	if !ok {
		return fmt.Errorf("RPC: %#v", x)
	}

	return nil
}*/

func (m *MTProto) pingRoutine() {
	defer func() {
		m.log.Debug("pingRoutine done")
		m.routinesWG.Done()
	}()
	for {
		select {
		case <-m.routinesStop:
			return
		case <-time.After(60 * time.Second):
			m.sendQueue <- newPacket(TL_ping{0xCADACADA}, nil)
		}
	}
}

func (m *MTProto) sendRoutine() {
	defer func() {
		m.log.Debug("sendRoutine done")
		m.routinesWG.Done()
	}()
	for {
		select {
		case <-m.routinesStop:
			return
		case x := <-m.sendQueue:
			err := m.send(x)
			if IsClosedConnErr(err) {
				continue //closed connection, should receive stop signal now
			}
			if err != nil {
				m.log.Error(err, "sending filed")
				go m.reconnectLogged()
				return
			}
		}
	}
}

func (m *MTProto) readRoutine() {
	defer func() {
		m.log.Debug("readRoutine done")
		m.routinesWG.Done()
	}()
	for {
		select {
		case <-m.routinesStop:
			return
		default:
		}

		data, err := m.read()
		if IsClosedConnErr(err) {
			continue //closed connection, should receive stop signal now
		}
		if err != nil {
			m.log.Error(err, "reading failed")
			go m.reconnectLogged()
			return
		}
		m.process(m.msgId, m.seqNo, data, true)
	}
}

func (m *MTProto) queueTransferRoutine() {
	defer func() {
		m.log.Debug("queueTransferRoutine done")
		m.routinesWG.Done()
	}()
	for {
		if len(m.sendQueue) < cap(m.extSendQueue) {
			select {
			case <-m.routinesStop:
				return
			case msg := <-m.extSendQueue:
				m.sendQueue <- msg
			}
		} else {
			select {
			case <-m.routinesStop:
				return
			default:
				time.Sleep(10000 * time.Microsecond)
			}
		}
	}
}

// Periodically checks messages in "msgsByID" and warns if they stay there too long
func (m *MTProto) debugRoutine() {
	for {
		m.mutex.Lock()
		count := 0
		for id := range m.msgsByID {
			delta := time.Now().Unix() - (id >> 32)
			if delta > 5 {
				m.log.Warn("msgsByID: #%d: is here for %ds", id, delta)
			}
			count++
		}
		m.mutex.Unlock()
		m.log.Debug("msgsByID: %d total", count)
		time.Sleep(5 * time.Second)
	}
}

func (m *MTProto) clearPacketData(msgID int64, response TL) {
	m.mutex.Lock()
	packet, ok := m.msgsByID[msgID]
	if ok {
		if packet.resp == nil {
			m.log.Warn("second response to message #%d %#v", msgID, packet.msg)
		} else {
			packet.resp <- response
			close(packet.resp)
			packet.resp = nil
		}
		delete(m.msgsByID, msgID)
	}
	m.mutex.Unlock()
}

func (m *MTProto) process(msgId int64, seqNo int32, dataTL TL, mayPassToHandler bool) {
	switch data := dataTL.(type) {
	case TL_msg_container:
		for _, v := range data.Items {
			m.process(v.MsgID, v.SeqNo, v.Data, true)
		}

	case TL_bad_server_salt:
		m.session.ServerSalt = data.NewServerSalt
		m.SaveSessionLogged()
		m.resendPendingPackets()

	case TL_bad_msg_notification:
		m.clearPacketData(data.BadMsgID, data)

	case TL_msgs_state_info:
		m.clearPacketData(data.ReqMsgID, data)

	case TL_new_session_created:
		m.session.ServerSalt = data.ServerSalt
		m.SaveSessionLogged()

	case TL_ping:
		m.sendQueue <- newPacket(TL_pong{msgId, data.PingID}, nil)

	case TL_pong:
		// (ignore) TODO

	case TL_msgs_ack:
		m.mutex.Lock()
		for _, id := range data.MsgIds {
			packet, ok := m.msgsByID[id]
			if ok {
				packet.needAck = false
				// if request does not wait for response, removing it
				if m.msgsByID[id].resp == nil {
					delete(m.msgsByID, id)
				}
			}
		}
		m.mutex.Unlock()

	case TL_rpc_result:
		m.process(msgId, 0, data.obj, false)
		m.clearPacketData(data.req_msg_id, data.obj)

	default:
		if mayPassToHandler && m.handleEvent != nil {
			go m.handleEvent(dataTL)
		}
	}

	// should acknowledge odd ids
	if (seqNo & 1) == 1 {
		m.sendQueue <- newPacket(TL_msgs_ack{[]int64{msgId}}, nil)
	}
}
