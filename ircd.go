package main

import (
	"crypto/tls"
	"fmt"
	"log"
	"net"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"summercat.com/irc"
)

// Catbox holds the state for this local server.
// I put everything global to a server in an instance of struct rather than
// have global variables.
type Catbox struct {
	// ConfigFile is the path to the config file.
	ConfigFile string

	// Config is the currently loaded config.
	Config *Config

	// Next client ID to issue. This turns into TS6 ID which gets concatenated
	// with our SID to make the TS6 UID. We wrap it in a mutex as different
	// goroutines must access it.
	NextClientID     uint64
	NextClientIDLock sync.Mutex

	// LocalClients are unregistered.
	// Client id (uint64) is the locally unique key.
	// It is useful to use this instead of TS6UID/TS6SID as we need to look up
	// all 3 types inside the event handler.
	// When we upgrade them to LocalUser/LocalServer, we drop them from this.
	LocalClients map[uint64]*LocalClient
	// LocalUsers are clients registered as users.
	LocalUsers map[uint64]*LocalUser
	// LocalServers are clients registered as servers.
	LocalServers map[uint64]*LocalServer

	// Users with operator status. They may be local or remote.
	Opers map[TS6UID]*User

	// Track nicks in use and by which user. Canonicalized nickname to TS6 UID.
	Nicks map[string]TS6UID

	// Track users on the network. TS6 UID to User. Local or remote.
	Users map[TS6UID]*User

	// Track servers on the network. TS6 SID to Server. Local or remote.
	Servers map[TS6SID]*Server

	// Track channels on the network. Channel name (canonicalized) to Channel.
	Channels map[string]*Channel

	// Active K:Lines (bans).
	KLines []KLine

	// When we close this channel, this indicates that we're shutting down.
	// Other goroutines can check if this channel is closed.
	ShutdownChan chan struct{}

	// Tell the server something on this channel.
	ToServerChan chan Event

	// The highest number of local users we have seen at once.
	HighestLocalUserCount int

	// The highest number of global users we have seen at once.
	HighestGlobalUserCount int

	// The highest number of local clients (unregistered + users + servers) we have
	// seen at once.
	HighestConnectionCount int

	// Track how many connections we've received in total.
	ConnectionCount int

	// CountersLock protects the above counters.
	CountersLock sync.Mutex

	// Our TLS configuration.
	TLSConfig *tls.Config

	// TCP plaintext and TLS listeners.
	Listener    net.Listener
	TLSListener net.Listener

	// WaitGroup to ensure all goroutines clean up before we end.
	WG sync.WaitGroup
}

// KLine holds a kline (a ban).
type KLine struct {
	// Together we have <usermask>@<hostmask>
	UserMask string
	HostMask string

	Reason string
}

// TS6ID is a client's unique identifier. Unique to this server only.
type TS6ID string

// TS6SID uniquely identifies a server. Globally.
type TS6SID string

// TS6UID is SID+UID. Uniquely identify a client. Globally.
type TS6UID string

// Event holds a message containing something to tell the server.
type Event struct {
	Type EventType

	Client *LocalClient

	Message irc.Message
}

// EventType is a type of event we can tell the server about.
type EventType int

const (
	// NullEvent is a default event. This means the event was not populated.
	NullEvent EventType = iota

	// NewClientEvent means a new client connected.
	NewClientEvent

	// DeadClientEvent means client died for some reason. Clean it up.
	// It's useful to be able to know immediately and inform the client if we're
	// going to decide they are getting cut off (e.g., malformed message).
	DeadClientEvent

	// MessageFromClientEvent means a client sent a message.
	MessageFromClientEvent

	// WakeUpEvent means the server should wake up and do bookkeeping.
	WakeUpEvent

	// RehashEvent tells the server to rehash.
	RehashEvent
)

// UserMessageLimit defines a cap on how many messages a user may send at once.
//
// As part of flood control, each user has a counter that maxes out at this
// number. Each message we process from them decrements their counter by one.
// If their counter reaches 0, we queue their message and process it once their
// counter becomes positive.
//
// Each second we raise each user's counter by one (to this maximum).
//
// This is similar to ircd-ratbox's flood control. See its packet.c.
const UserMessageLimit = 10

// ExcessFloodThreshold defines the number of messages a user may have queued
// before they get disconnected for flooding.
const ExcessFloodThreshold = 50

// ChanModesPerCommand tells how many channel modes we accept per MODE command
// from a user.
const ChanModesPerCommand = 4

func main() {
	log.SetFlags(0)

	args, err := getArgs()
	if err != nil {
		log.Fatal(err)
	}

	cb, err := newCatbox(args.ConfigFile)
	if err != nil {
		log.Fatal(err)
	}

	err = cb.start()
	if err != nil {
		log.Fatal(err)
	}

	log.Printf("Server shutdown cleanly.")
}

func newCatbox(configFile string) (*Catbox, error) {
	cb := Catbox{
		ConfigFile:   configFile,
		LocalClients: make(map[uint64]*LocalClient),
		LocalUsers:   make(map[uint64]*LocalUser),
		LocalServers: make(map[uint64]*LocalServer),
		Opers:        make(map[TS6UID]*User),
		Users:        make(map[TS6UID]*User),
		Nicks:        make(map[string]TS6UID),
		Servers:      make(map[TS6SID]*Server),
		Channels:     make(map[string]*Channel),
		KLines:       []KLine{},

		// shutdown() closes this channel.
		ShutdownChan: make(chan struct{}),

		// We never manually close this channel.
		ToServerChan: make(chan Event),
	}

	cfg, err := checkAndParseConfig(configFile)
	if err != nil {
		return nil, fmt.Errorf("Configuration problem: %s", err)
	}
	cb.Config = cfg

	cert, err := tls.LoadX509KeyPair(cb.Config.CertificateFile, cb.Config.KeyFile)
	if err != nil {
		return nil, fmt.Errorf("Unable to load certificate/key: %s", err)
	}

	tlsConfig := &tls.Config{
		Certificates:             []tls.Certificate{cert},
		PreferServerCipherSuites: true,
		SessionTicketsDisabled:   true,
		// Unfortunately it is usual to use self signed certificates with IRC. We
		// need this to connect to such servers.
		InsecureSkipVerify: true,

		// It would be nice to be able to be more restrictive on TLS version and
		// ciphers, but in practice many clients do not support the strictest.
		//CipherSuites: []uint16{
		//	tls.TLS_ECDHE_RSA_WITH_AES_256_GCM_SHA384,
		//	tls.TLS_ECDHE_ECDSA_WITH_AES_256_GCM_SHA384,
		//},
		//MinVersion: tls.VersionTLS12,
	}
	cb.TLSConfig = tlsConfig

	return &cb, nil
}

// start starts up the server.
//
// We open the TCP port, start goroutines, and then receive messages on our
// channels.
func (cb *Catbox) start() error {
	// Plaintext listener.
	if cb.Config.ListenPort != "" {
		ln, err := net.Listen("tcp", fmt.Sprintf("%s:%s", cb.Config.ListenHost,
			cb.Config.ListenPort))
		if err != nil {
			return fmt.Errorf("Unable to listen: %s", err)
		}
		cb.Listener = ln

		cb.WG.Add(1)
		go cb.acceptConnections(cb.Listener)
	}

	// TLS listener.
	if cb.Config.ListenPortTLS != "" {
		tlsLN, err := tls.Listen("tcp", fmt.Sprintf("%s:%s", cb.Config.ListenHost,
			cb.Config.ListenPortTLS), cb.TLSConfig)
		if err != nil {
			return fmt.Errorf("Unable to listen (TLS): %s", err)
		}
		cb.TLSListener = tlsLN

		cb.WG.Add(1)
		go cb.acceptConnections(cb.TLSListener)
	}

	// No ports set? Die!
	if cb.Config.ListenPort == "" && cb.Config.ListenPortTLS == "" {
		log.Fatalf("You must set at least one listen port.")
	}

	// Alarm is a goroutine to wake up this one periodically so we can do things
	// like ping clients.
	cb.WG.Add(1)
	go cb.alarm()

	// Catch SIGHUP and rehash.
	signalChan := make(chan os.Signal)
	signal.Notify(signalChan, syscall.SIGHUP)

	cb.WG.Add(1)
	go func() {
		defer cb.WG.Done()
		for {
			select {
			case <-signalChan:
				log.Printf("Received SIGHUP signal, rehashing")
				cb.newEvent(Event{Type: RehashEvent})
			case <-cb.ShutdownChan:
				signal.Stop(signalChan)
				// After Stop() we're guaranteed we will receive no more on the channel,
				// so we can close the channel, and then drain it.
				close(signalChan)
				for range signalChan {
				}
				log.Printf("Signal listener shutting down.")
				return
			}
		}
	}()

	cb.eventLoop()

	// We don't need to drain any channels. None close that will have any
	// goroutines blocked on them.

	cb.WG.Wait()

	return nil
}

// eventLoop processes events on the server's channel.
//
// It continues until the shutdown channel closes, indicating shutdown.
func (cb *Catbox) eventLoop() {
	for {
		select {
		// Careful about using the Client we get back in events. It may have been
		// promoted to a different client type (UserClient, ServerClient).
		case evt := <-cb.ToServerChan:
			if evt.Type == NewClientEvent {
				log.Printf("New client connection: %s", evt.Client)
				cb.LocalClients[evt.Client.ID] = evt.Client
				continue
			}

			if evt.Type == DeadClientEvent {
				lc, exists := cb.LocalClients[evt.Client.ID]
				if exists {
					lc.quit("I/O error")
					continue
				}
				lu, exists := cb.LocalUsers[evt.Client.ID]
				if exists {
					lu.quit("I/O error", true)
					continue
				}
				ls, exists := cb.LocalServers[evt.Client.ID]
				if exists {
					ls.quit("I/O error")
					continue
				}
				continue
			}

			if evt.Type == MessageFromClientEvent {
				lc, exists := cb.LocalClients[evt.Client.ID]
				if exists {
					lc.handleMessage(evt.Message)
					continue
				}
				lu, exists := cb.LocalUsers[evt.Client.ID]
				if exists {
					lu.handleMessage(evt.Message)
					continue
				}
				ls, exists := cb.LocalServers[evt.Client.ID]
				if exists {
					ls.handleMessage(evt.Message)
					continue
				}
				continue
			}

			if evt.Type == WakeUpEvent {
				cb.checkAndPingClients()
				cb.connectToServers()
				cb.floodControl()
				continue
			}

			if evt.Type == RehashEvent {
				cb.rehash(nil)
				continue
			}

			log.Fatalf("Unexpected event: %d", evt.Type)

		case <-cb.ShutdownChan:
			return
		}
	}
}

// shutdown starts server shutdown.
func (cb *Catbox) shutdown() {
	log.Printf("Server shutdown initiated.")

	// Closing ShutdownChan indicates to other goroutines that we're shutting
	// down.
	close(cb.ShutdownChan)

	err := cb.Listener.Close()
	if err != nil {
		log.Printf("Problem closing TCP listener: %s", err)
	}
	err = cb.TLSListener.Close()
	if err != nil {
		log.Printf("Problem closing TLS listener: %s", err)
	}

	// All clients need to be told. This also closes their write channels.
	for _, client := range cb.LocalClients {
		client.quit("Server shutting down")
	}
	for _, client := range cb.LocalServers {
		client.quit("Server shutting down")
	}
	for _, client := range cb.LocalUsers {
		client.quit("Server shutting down", false)
	}
}

// getClientID generates a new client ID. Each client that connects to us (or
// we connect to in the case of initiating a connection to a server) we assign
// a unique id using this function.
//
// We take a lock to allow it to be called safely from any goroutine.
func (cb *Catbox) getClientID() uint64 {
	cb.NextClientIDLock.Lock()
	defer cb.NextClientIDLock.Unlock()

	id := cb.NextClientID

	if cb.NextClientID+1 == 0 {
		log.Fatalf("Client id overflow")
	}
	cb.NextClientID++

	return id
}

// acceptConnections accepts TCP connections and tells the main server loop
// through a channel. It sets up separate goroutines for reading/writing to
// and from the client.
func (cb *Catbox) acceptConnections(listener net.Listener) {
	defer cb.WG.Done()

	for {
		if cb.isShuttingDown() {
			break
		}

		conn, err := listener.Accept()
		if err != nil {
			log.Printf("Failed to accept connection: %s", err)
			continue
		}

		cb.updateCounters(true)

		cb.introduceClient(conn)
	}

	log.Printf("Connection accepter shutting down.")
}

// introduceClient sets up a client we just accepted.
//
// It creates a Client struct, and sends initial NOTICEs to the client. It also
// attempts to look up the client's hostname.
func (cb *Catbox) introduceClient(conn net.Conn) {
	cb.WG.Add(1)

	go func() {
		defer cb.WG.Done()

		id := cb.getClientID()

		client := NewLocalClient(cb, id, conn)

		msgs := []string{
			fmt.Sprintf("*** Processing your connection to %s",
				cb.Config.ServerName),
		}

		tlsConn, ok := conn.(*tls.Conn)
		if ok {
			// Call Handshake as we may not have completed handshake yet. If not, we
			// are not able to have any useful connection state, so we can't tell them
			// their version and cipher.
			err := tlsConn.Handshake()
			if err != nil {
			}
			client.TLSConnectionState = tlsConn.ConnectionState()
			msgs = append(msgs, fmt.Sprintf("*** Connected with %s (%s)",
				tlsVersionToString(client.TLSConnectionState.Version),
				cipherSuiteToString(client.TLSConnectionState.CipherSuite)))
		}

		msgs = append(msgs, "*** Looking up your hostname...")

		hostname := lookupHostname(client.Conn.IP)
		if len(hostname) > 0 {
			msgs = append(msgs, "*** Found your hostname")
			client.Hostname = hostname
		} else {
			msgs = append(msgs, "*** Couldn't look up your hostname")
		}

		for _, msg := range msgs {
			client.WriteChan <- irc.Message{
				Command: "NOTICE",
				Params:  []string{"AUTH", msg},
			}
		}

		// Inform the main server goroutine about the client.
		// Do this after sending any messages to the client's channel as it is
		// possible the channel will be closed by the server (such as during
		// shutdown).
		cb.newEvent(Event{Type: NewClientEvent, Client: client})

		// Start read goroutine (endlessly read messages from the client) and write
		// goroutine (endlessly write messages to the client).
		cb.WG.Add(1)
		go client.readLoop()

		cb.WG.Add(1)
		go client.writeLoop()
	}()
}

// Return true if the server is shutting down.
func (cb *Catbox) isShuttingDown() bool {
	// No messages get sent to this channel, so if we receive a message on it,
	// then we know the channel was closed.
	select {
	case <-cb.ShutdownChan:
		return true
	default:
		return false
	}
}

// Alarm sends a message to the server goroutine to wake it up.
// It sleeps and then repeats.
func (cb *Catbox) alarm() {
	defer cb.WG.Done()

	for {
		if cb.isShuttingDown() {
			break
		}

		// We need to wake up every second for flood control.
		time.Sleep(time.Second)

		cb.newEvent(Event{Type: WakeUpEvent})
	}

	log.Printf("Alarm shutting down.")
}

// checkAndPingClients looks at each connected client.
//
// If they've been idle a short time, we send them a PING (if they're
// registered).
//
// If they've been idle a long time, we kill their connection.
//
// We also kill any whose send queue maxed out.
func (cb *Catbox) checkAndPingClients() {
	now := time.Now()

	// Unregistered clients do not receive PINGs, nor do we care about their
	// idle time. Kill them if they are connected too long and still unregistered.
	for _, client := range cb.LocalClients {
		if client.SendQueueExceeded {
			client.quit("SendQ exceeded")
			continue
		}

		timeConnected := now.Sub(client.ConnectionStartTime)

		// If it's been connected long enough to need to ping it, cut it off.
		if timeConnected > cb.Config.PingTime {
			client.quit("Idle too long.")
		}
	}

	// User and server clients we are more lenient with. Ping them if they are
	// idle for a while.

	for _, client := range cb.LocalUsers {
		if client.SendQueueExceeded {
			client.quit("SendQ exceeded", true)
			continue
		}

		timeIdle := now.Sub(client.LastActivityTime)

		// Was it active recently enough that we don't need to do anything?
		if timeIdle < cb.Config.PingTime {
			continue
		}

		// It's been idle a while.

		// Has it been idle long enough that we consider it dead?
		if timeIdle > cb.Config.DeadTime {
			client.quit(fmt.Sprintf("Ping timeout: %d seconds",
				int(timeIdle.Seconds())), true)
			continue
		}

		timeSincePing := now.Sub(client.LastPingTime)

		// Should we ping it? We might have pinged it recently.
		if timeSincePing < cb.Config.PingTime {
			continue
		}

		client.messageFromServer("PING", []string{cb.Config.ServerName})
		client.LastPingTime = now
		continue
	}

	for _, server := range cb.LocalServers {
		if server.SendQueueExceeded {
			server.quit("SendQ exceeded")
			continue
		}

		// If it is bursting then we want to check it doesn't go on too long. Drop
		// it if it does.
		if server.Bursting {
			timeConnected := now.Sub(server.ConnectionStartTime)

			if timeConnected > cb.Config.PingTime {
				server.quit("Bursting too long")
			}
			continue
		}

		// Its burst completed. Now we monitor the last time we heard from it
		// and possibly ping it.

		timeIdle := now.Sub(server.LastActivityTime)

		// Was it active recently enough that we don't need to do anything?
		if timeIdle < cb.Config.PingTime {
			continue
		}

		// It's been idle a while.

		// Has it been idle long enough that we consider it dead?
		if timeIdle > cb.Config.DeadTime {
			server.quit(fmt.Sprintf("Ping timeout: %d seconds",
				int(timeIdle.Seconds())))
			continue
		}

		timeSincePing := now.Sub(server.LastPingTime)

		// Should we ping it? We might have pinged it recently.
		if timeSincePing < cb.Config.PingTime {
			continue
		}

		// PING origin is our SID for servers.
		server.messageFromServer("PING", []string{string(cb.Config.TS6SID)})
		server.LastPingTime = now
		continue
	}
}

// connectToServers looks at our defined server links. If we are not connected
// to any, it attempts to initiate a connection.
//
// It tries each server only once per configured period. i.e., if you call this
// function repeatedly, it may not try to connect to a server twice if it failed
func (cb *Catbox) connectToServers() {
	now := time.Now()

	for _, linkInfo := range cb.Config.Servers {
		// It does not make sense to try to connect to ourself. Even if we're in the
		// config.
		if linkInfo.Name == cb.Config.ServerName {
			continue
		}

		if cb.isLinkedToServer(linkInfo.Name) {
			continue
		}

		timeSinceLastAttempt := now.Sub(linkInfo.LastConnectAttempt)

		if timeSinceLastAttempt < cb.Config.ConnectAttemptTime {
			continue
		}

		linkInfo.LastConnectAttempt = now
		cb.connectToServer(linkInfo)
	}
}

// floodControl updates the message counters for all users, and potentially
// processes queued messages for any that hit their limit.
//
// Each user will have its message counter increased by 1 to a maximum of
// UserMessageLimit.
//
// Each user will have its queued messages processed until their message counter
// hits zero.
//
// If a user has too many queued messages, we cut them off for excess flooding,
// but that does not happen here. It happens where we add to the queue. This is
// to try to kill clients that might otherwise overwhelm us.
//
// We expect to be called every ~second.
//
// Even if a user is flood exempt, continue checking them here. The reason is
// if they became an operator, we want to make sure we process any queued
// messages they may have before that.
func (cb *Catbox) floodControl() {
	for _, user := range cb.LocalUsers {
		// Bump up their message counter by one if they are not maxed out.
		if user.MessageCounter < UserMessageLimit {
			user.MessageCounter++
		}

		// Process their queued messages until their message counter hits zero.
		for user.MessageCounter > 0 && len(user.MessageQueue) > 0 {
			// Pull a message off the queue.
			msg := user.MessageQueue[0]
			user.MessageQueue = user.MessageQueue[1:]

			// Process it.
			// handleMessage decrements our message counter.
			user.handleMessage(msg)
		}
	}
}

// Determine if we are linked to a given server.
func (cb *Catbox) isLinkedToServer(name string) bool {
	// We're always linked to ourself.
	if name == cb.Config.ServerName {
		return true
	}

	for _, server := range cb.Servers {
		if server.Name == name {
			return true
		}
	}
	return false
}

// Initiate a connection to a server.
//
// Does this in a goroutine to avoid blocking server goroutine.
func (cb *Catbox) connectToServer(linkInfo *ServerDefinition) {
	cb.WG.Add(1)

	go func() {
		defer cb.WG.Done()

		var conn net.Conn
		var err error

		if linkInfo.TLS {
			cb.noticeOpers(fmt.Sprintf("Connecting to %s with TLS...", linkInfo.Name))

			dialer := &net.Dialer{
				Timeout: cb.Config.DeadTime,
			}
			conn, err = tls.DialWithDialer(dialer, "tcp",
				fmt.Sprintf("%s:%d", linkInfo.Hostname, linkInfo.Port),
				cb.TLSConfig)
		} else {
			cb.noticeOpers(fmt.Sprintf("Connecting to %s without TLS...",
				linkInfo.Name))
			conn, err = net.DialTimeout("tcp",
				fmt.Sprintf("%s:%d", linkInfo.Hostname, linkInfo.Port),
				cb.Config.DeadTime)
		}

		if err != nil {
			cb.noticeOpers(fmt.Sprintf("Unable to connect to server [%s]: %s",
				linkInfo.Name, err))
			return
		}

		id := cb.getClientID()

		client := NewLocalClient(cb, id, conn)

		// Make sure we send to the client's write channel before telling the server
		// about the client. It is possible otherwise that the server (if shutting
		// down) could have closed the write channel on us.
		client.sendServerIntro(linkInfo.Pass)

		cb.newEvent(Event{Type: NewClientEvent, Client: client})

		cb.WG.Add(1)
		go client.readLoop()

		cb.WG.Add(1)
		go client.writeLoop()
	}()
}

// newEvent tells the server something happens.
//
// Any goroutine can call this function.
//
// It sends the server a message on its ToServerChan.
//
// It will not block on shutdown as we select on the shutdown channel which we
// close when shutting down the server. This means receive on the shutdown
// channel will proceed at that point.
//
// We only need to use this function in goroutines other the main server
// goroutine.
func (cb *Catbox) newEvent(evt Event) {
	select {
	case cb.ToServerChan <- evt:
	case <-cb.ShutdownChan:
	}
}

// Send a message to all operator users.
func (cb *Catbox) noticeOpers(msg string) {
	log.Printf("Global oper notice: %s", msg)

	for _, user := range cb.Opers {
		if user.isLocal() {
			user.LocalUser.serverNotice(msg)
			continue
		}

		user.ClosestServer.maybeQueueMessage(irc.Message{
			Prefix:  string(cb.Config.TS6SID),
			Command: "NOTICE",
			Params: []string{
				string(user.UID),
				fmt.Sprintf("*** Notice --- %s", msg),
			},
		})
	}
}

// Send a message to all local operator users.
func (cb *Catbox) noticeLocalOpers(msg string) {
	log.Printf("Local oper notice: %s", msg)

	for _, user := range cb.Opers {
		if user.isLocal() {
			user.LocalUser.serverNotice(msg)
			continue
		}
	}
}

// Store a KLINE locally, and then check if any connected local users match
// it. If so, cut them off and notify local opers.
//
// This function does not propagate to any other servers.
//
// KLines are currently always permanent locally.
func (cb *Catbox) addAndApplyKLine(kline KLine, source, reason string) {
	// If it's a duplicate KLINE, ignore it.
	for _, k := range cb.KLines {
		if k.UserMask != kline.UserMask {
			continue
		}
		if k.HostMask != kline.HostMask {
			continue
		}
		cb.noticeOpers(fmt.Sprintf("Ignoring duplicate K-Line for [%s@%s] from %s",
			k.UserMask, k.HostMask, source))
		return
	}

	cb.KLines = append(cb.KLines, kline)

	cb.noticeOpers(fmt.Sprintf("%s added K-Line for [%s@%s] [%s]",
		source, kline.UserMask, kline.HostMask, reason))

	// Do we have any matching users connected? Cut them off if so.

	quitReason := fmt.Sprintf("Connection closed: %s", reason)

	for _, user := range cb.LocalUsers {
		if !user.User.matchesMask(kline.UserMask, kline.HostMask) {
			continue
		}

		user.quit(quitReason, true)

		cb.noticeOpers(fmt.Sprintf("User disconnected due to K-Line: %s",
			user.User.DisplayNick))
	}
}

func (cb *Catbox) removeKLine(userMask, hostMask, source string) bool {
	idx := -1
	for i, kline := range cb.KLines {
		if kline.UserMask != userMask || kline.HostMask != hostMask {
			continue
		}
		idx = i
		break
	}

	if idx == -1 {
		cb.noticeOpers(fmt.Sprintf("Not removing K-Line for [%s@%s] (not found)",
			userMask, hostMask))
		return false
	}

	cb.KLines = append(cb.KLines[:idx], cb.KLines[idx+1:]...)

	cb.noticeOpers(fmt.Sprintf("%s removed K-Line for [%s@%s]",
		source, userMask, hostMask))

	return true
}

// Issue a KILL from this server.
//
// We send a KILL message to each server.
//
// We cut the user off if they are local.
//
// We tell local users a Quit if they are remote.
//
// We forget the user.
//
// If byUser is nil, then this is a server KILL. Source will be the server
// name.
func (cb *Catbox) issueKill(byUser, u *User, message string) {
	// Parameters: <target user UID> <reason>
	// Reason has format:
	// <source> (<reason text>)
	// Where <source> looks something like:
	// <killer server name>!<killer user host>!<killer user username>!<killer nick>

	reason := ""
	killer := ""
	if byUser == nil {
		reason = fmt.Sprintf("%s (%s)", cb.Config.ServerName, message)
		killer = cb.Config.ServerName
	} else {
		reason = fmt.Sprintf("%s!%s!%s!%s (%s)", cb.Config.ServerName,
			byUser.Hostname, byUser.Username, byUser.DisplayNick, message)
		killer = byUser.DisplayNick
	}

	// Send to all servers.
	for _, server := range cb.LocalServers {
		server.maybeQueueMessage(irc.Message{
			Prefix:  string(cb.Config.TS6SID),
			Command: "KILL",
			Params:  []string{string(u.UID), reason},
		})
	}

	// Tell all opers about it.
	cb.noticeOpers(fmt.Sprintf("Received KILL message for %s. From %s (%s)",
		u.DisplayNick, killer, message))

	quitReason := fmt.Sprintf("Killed (%s (%s))", killer, message)

	// If it's a local user, drop it.
	if u.isLocal() {
		// We don't need to propagate a QUIT. We'll be sending a KILL.
		u.LocalUser.quit(quitReason, false)
		return
	}

	// It's a remote user. Tell local users a quit message.
	// And forget the remote user.
	cb.quitRemoteUser(u, quitReason)
}

// Build irc.Messages that make up a WHOIS response. You can then send them to
// where they need to go.
//
// You should call this only if the user exists.
//
// If useIDs is true, then we set the messages to use SID/UID where they would
// otherwise use server name and nickname.
// Note this applies to the source server and target user, not the 2nd
// parameter.
func (cb *Catbox) createWHOISResponse(user, replyUser *User,
	useIDs bool) []irc.Message {
	msgs := []irc.Message{}

	from := cb.Config.ServerName
	if useIDs {
		from = string(cb.Config.TS6SID)
	}

	to := replyUser.DisplayNick
	if useIDs {
		to = string(replyUser.UID)
	}

	// 311 RPL_WHOISUSER
	msgs = append(msgs, irc.Message{
		Prefix:  from,
		Command: "311",
		Params: []string{
			to,
			user.DisplayNick,
			user.Username,
			user.Hostname,
			"*",
			user.RealName,
		},
	})

	// 319 RPL_WHOISCHANNELS
	// I choose to not show any.

	// 312 RPL_WHOISSERVER
	msgs = append(msgs, irc.Message{
		Prefix:  from,
		Command: "312",
		Params: []string{
			to,
			user.DisplayNick,
			cb.Config.ServerName,
			cb.Config.ServerInfo,
		},
	})

	// 301 RPL_AWAY.
	if len(user.AwayMessage) > 0 {
		msgs = append(msgs, irc.Message{
			Prefix:  from,
			Command: "301",
			Params: []string{
				to,
				user.DisplayNick,
				user.AwayMessage,
			},
		})
	}

	// 313 RPL_WHOISOPERATOR
	if user.isOperator() {
		msgs = append(msgs, irc.Message{
			Prefix:  from,
			Command: "313",
			Params: []string{
				to,
				user.DisplayNick,
				"is an IRC operator",
			},
		})
	}

	// 671. Non standard. Ratbox uses it.
	if user.isLocal() && user.LocalUser.isTLS() {
		msgs = append(msgs, irc.Message{
			Prefix:  from,
			Command: "671",
			Params: []string{
				to,
				user.DisplayNick,
				fmt.Sprintf("is using a secure connection (%s) (%s)",
					tlsVersionToString(user.LocalUser.TLSConnectionState.Version),
					cipherSuiteToString(user.LocalUser.TLSConnectionState.CipherSuite)),
			},
		})
	}

	// 317 RPL_WHOISIDLE. Only if local.
	if user.isLocal() {
		idleDuration := time.Now().Sub(user.LocalUser.LastMessageTime)
		idleSeconds := int(idleDuration.Seconds())

		msgs = append(msgs, irc.Message{
			Prefix:  from,
			Command: "317",
			Params: []string{
				to,
				user.DisplayNick,
				fmt.Sprintf("%d", idleSeconds),
				// Adding a signon time is non standard, but apparently common.
				fmt.Sprintf("%d", user.LocalUser.ConnectionStartTime.Unix()),
				"seconds idle, signon time",
			},
		})
	}

	// 318 RPL_ENDOFWHOIS
	msgs = append(msgs, irc.Message{
		Prefix:  from,
		Command: "318",
		Params: []string{
			to,
			user.DisplayNick,
			"End of WHOIS list",
		},
	})

	return msgs
}

// Update our counters.
//
// We track the maximum number of local users we've seen, and the maximum number
// of global users we've seen.
//
// The main reason is to show in LUSERS output.
//
// You should call this after you have made any changes to clients/users/servers
// counts.
//
// If this adding a new client connection, pass newClient as true.
// This should only be the case if we just accepted a new connection locally.
func (cb *Catbox) updateCounters(newClient bool) {
	cb.CountersLock.Lock()
	defer cb.CountersLock.Unlock()

	if len(cb.LocalUsers) > cb.HighestLocalUserCount {
		cb.HighestLocalUserCount = len(cb.LocalUsers)
	}

	if len(cb.Users) > cb.HighestGlobalUserCount {
		cb.HighestGlobalUserCount = len(cb.Users)
	}

	currentClientCount := len(cb.LocalClients) + len(cb.LocalUsers) +
		len(cb.LocalServers)
	if currentClientCount > cb.HighestConnectionCount {
		cb.HighestConnectionCount = currentClientCount
	}

	if newClient {
		cb.ConnectionCount++
	}
}

// We're losing a remote user.
//
// Inform local users that it is leaving with a QUIT message.
//
// Forget the user from all records.
func (cb *Catbox) quitRemoteUser(u *User, message string) {
	// Remove the user from each channel.
	// Also, tell each local client that is in 1+ channel with the user that this
	// user quit.
	informedUsers := make(map[TS6UID]struct{})

	quitParams := []string{}
	if len(message) > 0 {
		quitParams = append(quitParams, message)
	}

	for _, channel := range u.Channels {
		for memberUID := range channel.Members {
			member := cb.Users[memberUID]
			if !member.isLocal() {
				continue
			}

			_, exists := informedUsers[member.UID]
			if exists {
				continue
			}
			informedUsers[member.UID] = struct{}{}

			member.LocalUser.maybeQueueMessage(irc.Message{
				Prefix:  u.nickUhost(),
				Command: "QUIT",
				Params:  quitParams,
			})
		}

		channel.removeUser(u)
		if len(channel.Members) == 0 {
			delete(cb.Channels, channel.Name)
		}
	}

	// Forget the user.
	delete(cb.Users, u.UID)
	if u.isOperator() {
		delete(cb.Opers, u.UID)
	}
	delete(cb.Nicks, canonicalizeNick(u.DisplayNick))
}

// Rehash reloads our config.
//
// Only certain config options can change during rehash.
//
// We could close listeners and open new ones. But nah.
func (cb *Catbox) rehash(byUser *User) {
	cfg, err := checkAndParseConfig(cb.ConfigFile)
	if err != nil {
		cb.noticeOpers(fmt.Sprintf("Rehash: Configuration problem: %s", err))
		return
	}

	cb.Config.MOTD = cfg.MOTD
	cb.Config.Opers = cfg.Opers
	cb.Config.Servers = cfg.Servers
	cb.Config.UserConfigs = cfg.UserConfigs

	if byUser != nil {
		cb.noticeOpers(fmt.Sprintf("%s rehashed configuration.",
			byUser.DisplayNick))
	} else {
		cb.noticeOpers("Rehashed configuration.")
	}
}

// Look up a server by its name. e.g., irc.example.com
func (cb *Catbox) getServerByName(name string) *Server {
	for _, server := range cb.Servers {
		if server.Name == name {
			return server
		}
	}
	return nil
}

// Send a message to all local users in a channel.
func (cb *Catbox) messageLocalUsersOnChannel(channel *Channel, m irc.Message) {
	for memberUID := range channel.Members {
		member := cb.Users[memberUID]

		if !member.isLocal() {
			continue
		}

		member.LocalUser.maybeQueueMessage(m)
	}
}
