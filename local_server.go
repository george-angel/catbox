package main

import (
	"fmt"
	"log"
	"strconv"
	"strings"
	"time"

	"summercat.com/irc"
)

// LocalServer means the client registered as a server. This holds its info.
type LocalServer struct {
	*LocalClient

	Server *Server

	Capabs map[string]struct{}

	// The last time we heard anything from it.
	LastActivityTime time.Time

	// The last time we sent it a PING.
	LastPingTime time.Time

	// Flags to know about our bursting state.
	GotPING  bool
	GotPONG  bool
	Bursting bool
}

// NewLocalServer upgrades a LocalClient to a LocalServer.
func NewLocalServer(c *LocalClient) *LocalServer {
	now := time.Now()

	s := &LocalServer{
		LocalClient:      c,
		Capabs:           c.PreRegCapabs,
		LastActivityTime: now,
		LastPingTime:     now,
		GotPING:          false,
		GotPONG:          false,
		Bursting:         true,
	}

	return s
}

func (s *LocalServer) String() string {
	return fmt.Sprintf("%s %s", s.Server.String(), s.Conn.RemoteAddr())
}

func (s *LocalServer) messageFromServer(command string, params []string) {
	// For numeric messages, we need to prepend the nick.
	// Use * for the nick in cases where the client doesn't have one yet.
	// This is what ircd-ratbox does. Maybe not RFC...
	if isNumericCommand(command) {
		newParams := []string{string(s.Server.SID)}
		newParams = append(newParams, params...)
		params = newParams
	}

	s.maybeQueueMessage(irc.Message{
		Prefix:  string(s.Catbox.Config.TS6SID),
		Command: command,
		Params:  params,
	})
}

func (s *LocalServer) quit(msg string) {
	// May already be cleaning up.
	_, exists := s.Catbox.LocalServers[s.ID]
	if !exists {
		return
	}

	// When quitting, you may think we should send SQUIT to all servers.
	// But we don't. Or ircd-ratbox does not. Do the same.
	// Just send it to our local servers, they propagate it.
	s.messageFromServer("ERROR", []string{msg})

	close(s.WriteChan)

	s.serverSplitCleanUp(s.Server)

	// Inform other servers that we are connected to.
	for _, server := range s.Catbox.LocalServers {
		server.maybeQueueMessage(irc.Message{
			Prefix:  string(s.Catbox.Config.TS6SID),
			Command: "SQUIT",
			Params:  []string{string(s.Server.SID), msg},
		})
	}

	s.Catbox.noticeLocalOpers(fmt.Sprintf("Server %s delinked: %s",
		s.Server.Name, msg))
}

// lostServer is departing the network.
//
// Inform all local users of QUITs for clients on the other side.
//
// Also do our local bookkeeping:
// - Forget the clients on the other side
// - Forget the servers on the other side
// - Forget the server
//
// This can happen when a local server delinks from us, or we're hearing about
// a server departing remotely (from a SQUIT command).
//
// This function does not propagate any messages to any servers. It only sends
// messages to local clients.
func (s *LocalServer) serverSplitCleanUp(lostServer *Server) {
	// The server may have been linked to other servers. Figure out all servers
	// we're losing.
	lostServers := lostServer.getLinkedServers(s.Catbox.Servers)

	// Include the one we're losing with its links.
	lostServers = append(lostServers, lostServer)

	// Look for users we are losing.
	for _, user := range s.Catbox.Users {
		if user.isLocal() {
			continue
		}

		// Are we losing this user?
		// We are if it is on a server we are losing.
		keepingUser := true
		for _, server := range lostServers {
			if user.Server == server {
				keepingUser = false
			}
		}

		if keepingUser {
			continue
		}

		log.Printf("Losing user %s", user)

		// This user is gone.

		// Tell local users about them quitting.
		// Remote users will be told by their own servers.

		// Quit message format is important. It tells that there was a netsplit,
		// and between which two servers.
		var quitMessage string
		if lostServer.isLocal() {
			quitMessage = fmt.Sprintf("%s %s", s.Catbox.Config.ServerName,
				lostServer.Name)
		} else {
			quitMessage = fmt.Sprintf("%s %s", lostServer.LinkedTo.Name,
				lostServer.Name)
		}

		s.Catbox.quitRemoteUser(user, quitMessage)
	}

	// Forget all lost servers.
	for _, server := range lostServers {
		log.Printf("Losing server %s", server)
		if server.isLocal() {
			delete(s.Catbox.LocalServers, server.LocalServer.ID)
		}
		delete(s.Catbox.Servers, server.SID)
	}
}

// Send the burst. This tells the server about the state of the world as we see
// it.
// We send our burst after seeing SVINFO. This means we have not yet processed
// any SID, UID, or SJOIN messages from the other side.
func (s *LocalServer) sendBurst() {
	// Tell it about all servers we know about.
	// Use the SID command.
	//
	// We do tell it about servers even if they are not directly linked to us.
	//
	// We need to be sure we set the prefix/source correctly to indicate what
	// server they are linked to.
	//
	// Parameters: <server name> <hop count> <SID> <description>
	// e.g.: :8ZZ SID irc3.example.com 2 9ZQ :My Desc
	//
	// It's also critical the order we inform the server about other servers.
	// If we tell it about server B linked to server C (i.e., prefix is server C)
	// but we haven't told it about server C yet, then it does not have sufficient
	// information to validate the server. The server could take it on faith that
	// it will be told about server C shortly, but that is not very good.
	//
	// We can accomplish this through telling it about servers ordered by hopcount
	// ascending.
	servers := sortServersByHopCount(s.Catbox.Servers)
	for _, server := range servers {
		// Don't send it itself.
		if server.LocalServer == s {
			continue
		}

		var linkedTo TS6SID
		if server.isLocal() {
			linkedTo = s.Catbox.Config.TS6SID
		} else {
			linkedTo = server.LinkedTo.SID
		}

		s.maybeQueueMessage(irc.Message{
			Prefix:  string(linkedTo),
			Command: "SID",
			Params: []string{
				server.Name,
				fmt.Sprintf("%d", server.HopCount+1),
				string(server.SID),
				server.Description,
			},
		})

		// Tell it about the capabilities of each server too. ratbox does this
		// during server link.
		s.maybeQueueMessage(irc.Message{
			Prefix:  string(server.SID),
			Command: "ENCAP",
			Params:  []string{"*", "GCAP", server.capabsString()},
		})
	}

	// Tell it about all users we know about. Use the UID command.
	// Ensure we set the prefix/source to the server it is on.
	// Parameters: <nick> <hopcount> <nick TS> <umodes> <username> <hostname> <IP> <UID> :<real name>
	// :8ZZ UID will 1 1475024621 +i will blashyrkh. 0 8ZZAAAAAB :will
	for _, user := range s.Catbox.Users {
		var onServer TS6SID
		if user.isLocal() {
			onServer = s.Catbox.Config.TS6SID
		} else {
			onServer = user.Server.SID
		}
		s.maybeQueueMessage(irc.Message{
			Prefix:  string(onServer),
			Command: "UID",
			Params: []string{
				user.DisplayNick,
				// Hop count increases for them.
				fmt.Sprintf("%d", user.HopCount+1),
				fmt.Sprintf("%d", user.NickTS),
				user.modesString(),
				user.Username,
				user.Hostname,
				user.IP,
				string(user.UID),
				user.RealName,
			},
		})

		// Send AWAY if they are away.
		if len(user.AwayMessage) == 0 {
			continue
		}
		s.maybeQueueMessage(irc.Message{
			Prefix:  string(user.UID),
			Command: "AWAY",
			Params:  []string{user.AwayMessage},
		})
	}

	// Send channels and the users in them with SJOIN commands.
	// Parameters: <channel TS> <channel name> <modes> [mode params] :<UIDs>
	// e.g., :8ZZ SJOIN 1475187553 #test2 +sn :@8ZZAAAAAB
	// Each UID may be prefixed with @ and/or + if voiced/opped.

	for _, channel := range s.Catbox.Channels {
		// We want to combine as many UIDs into a single SJOIN message as possible.

		// First make a message with what is common to all messages so that we can
		// determine the base length.
		sjoinMessage := irc.Message{
			Prefix:  string(s.Catbox.Config.TS6SID),
			Command: "SJOIN",
			Params: []string{
				fmt.Sprintf("%d", channel.TS),
				channel.Name,
				// Currently we only support +ns.
				"+ns",
				// UIDs go in the last parameter. As it is blank, encoding will turn it
				// into " :" for us. This is acceptable.
				"",
			},
		}

		sjoinEncoded, err := sjoinMessage.Encode()
		if err != nil {
			s.quit(fmt.Sprintf("Unable to create SJOIN message: %s", err))
			return
		}

		baseSize := len(sjoinEncoded)

		uids := ""
		for uid := range channel.Members {
			uidStr := string(uid)

			// Assume the first may fit.
			if len(uids) == 0 {
				uids += uidStr
				continue
			}

			// If we'll exceed the max protocol message length, fire the message and
			// start a new list.
			// +1 to account for a space.
			if baseSize+len(uids)+1+len(uidStr) > irc.MaxLineLength {
				sjoinMessage.Params[3] = uids
				s.maybeQueueMessage(sjoinMessage)
				uids = "" + uidStr
				continue
			}

			// Add it to the list.
			uids += " " + uidStr
		}

		if len(uids) > 0 {
			sjoinMessage.Params[3] = uids
			s.maybeQueueMessage(sjoinMessage)
		}

		// If they support the TB capab then send them TB commands. This tells them
		// the topic for each channel.
		if s.Server.hasCapability("TB") && len(channel.Topic) > 0 {
			s.maybeQueueMessage(irc.Message{
				Prefix:  string(s.Catbox.Config.TS6SID),
				Command: "TB",
				Params: []string{
					channel.Name,
					fmt.Sprintf("%d", channel.TopicTS),
					channel.TopicSetter,
					channel.Topic,
				},
			})
		}
	}

}

// The server sent us a message. Deal with it.
func (s *LocalServer) handleMessage(m irc.Message) {
	// Record that client said something to us just now.
	s.LastActivityTime = time.Now()

	// Ensure we always have a prefix. It removes the need to check this
	// elsewhere.
	if len(m.Prefix) == 0 {
		m.Prefix = string(s.Server.SID)
	}

	if m.Command == "PING" {
		s.pingCommand(m)
		return
	}

	if m.Command == "PONG" {
		s.pongCommand(m)
		return
	}

	if m.Command == "ERROR" {
		s.errorCommand(m)
		return
	}

	if m.Command == "UID" {
		s.uidCommand(m)
		return
	}

	if m.Command == "PRIVMSG" || m.Command == "NOTICE" {
		s.privmsgCommand(m)
		return
	}

	if m.Command == "SID" {
		s.sidCommand(m)
		return
	}

	if m.Command == "SJOIN" {
		s.sjoinCommand(m)
		return
	}

	if m.Command == "TB" {
		s.tbCommand(m)
		return
	}

	if m.Command == "JOIN" {
		s.joinCommand(m)
		return
	}

	if m.Command == "NICK" {
		s.nickCommand(m)
		return
	}

	if m.Command == "PART" {
		s.partCommand(m)
		return
	}

	// ircd-ratbox sends OPERWALL between servers, like WALLOPS
	if m.Command == "WALLOPS" || m.Command == "OPERWALL" {
		s.wallopsCommand(m)
		return
	}

	if m.Command == "QUIT" {
		s.quitCommand(m)
		return
	}

	if m.Command == "MODE" {
		s.modeCommand(m)
		return
	}

	if m.Command == "TOPIC" {
		s.topicCommand(m)
		return
	}

	if m.Command == "SQUIT" {
		s.squitCommand(m)
		return
	}

	if m.Command == "KILL" {
		s.killCommand(m)
		return
	}

	if m.Command == "ENCAP" {
		s.encapCommand(m)
		return
	}

	if m.Command == "WHOIS" {
		s.whoisCommand(m)
		return
	}

	if isNumericCommand(m.Command) {
		s.numericCommand(m)
		return
	}

	if m.Command == "CLICONN" {
		s.cliconnCommand(m)
		return
	}

	if m.Command == "AWAY" {
		s.awayCommand(m)
		return
	}

	if m.Command == "INVITE" {
		s.inviteCommand(m)
		return
	}

	// 421 ERR_UNKNOWNCOMMAND
	s.messageFromServer("421", []string{m.Command, "Unknown command"})
}

// We expect a PING from server as part of burst end. It also happens
// periodically.
func (s *LocalServer) pingCommand(m irc.Message) {
	// PING <origin name> [Destination SID]
	if len(m.Params) < 1 {
		// 461 ERR_NEEDMOREPARAMS
		s.messageFromServer("461", []string{"PING", "Not enough parameters"})
		return
	}

	// :9ZQ PING irc3.example.com :000
	// Where irc3.example.com == 9ZQ and it is remote

	// We want to send back
	// :000 PONG irc.example.com :9ZQ

	// I don't use origin name. Instead, look only at the prefix.

	sourceSID := TS6SID(m.Prefix)

	// Do we know the server making the ping request?
	_, exists := s.Catbox.Servers[sourceSID]
	if !exists {
		// 402 ERR_NOSUCHSERVER
		s.maybeQueueMessage(irc.Message{
			Prefix:  string(s.Catbox.Config.TS6SID),
			Command: "402",
			Params:  []string{string(sourceSID), "No such server"},
		})
		return
	}

	// Who's the destination of the ping? Default to us if there is none set.
	destinationSID := s.Catbox.Config.TS6SID
	if len(m.Params) >= 2 {
		destinationSID = TS6SID(m.Params[1])
	}

	// If it's for us, reply.
	// If it's not for us, propagate it to where it should go.

	if destinationSID == s.Catbox.Config.TS6SID {
		s.maybeQueueMessage(irc.Message{
			Prefix:  string(s.Catbox.Config.TS6SID),
			Command: "PONG",
			Params:  []string{s.Catbox.Config.ServerName, string(sourceSID)},
		})

		// If we're bursting, is it over? We expect to be PINGed at the end of their
		// burst.
		if s.Bursting && sourceSID == s.Server.SID {
			s.GotPING = true
			if s.GotPONG {
				s.Bursting = false
				s.Catbox.noticeOpers(fmt.Sprintf("Burst with %s over.", s.Server.Name))
			}
		}
		return
	}

	// Propagate it to where it should go.
	destServer, exists := s.Catbox.Servers[destinationSID]
	if !exists {
		// 402 ERR_NOSUCHSERVER
		s.maybeQueueMessage(irc.Message{
			Prefix:  string(s.Catbox.Config.TS6SID),
			Command: "402",
			Params:  []string{string(destinationSID), "No such server"},
		})
		return
	}

	if destServer.isLocal() {
		destServer.LocalServer.maybeQueueMessage(m)
		return
	}
	destServer.ClosestServer.maybeQueueMessage(m)
}

func (s *LocalServer) pongCommand(m irc.Message) {
	// We expect this at end of server link burst.
	// :<Remote SID> PONG <Remote server name> <My SID>
	// However we can also get it afterwards and may need to propagate it.
	if len(m.Params) < 2 {
		// 461 ERR_NEEDMOREPARAMS
		s.messageFromServer("461", []string{"PONG", "Not enough parameters"})
		return
	}

	// Check the source of the PONG.
	_, exists := s.Catbox.Servers[TS6SID(m.Prefix)]
	if !exists {
		s.quit("Unknown source server (PONG)")
		return
	}

	// We don't need to look at the remote server name. It should be referring to
	// the same server as the source SID.

	// The destination for the PONG.
	destinationSID := TS6SID(m.Params[1])

	// If it's for us, just accept it. There's no need to reply.
	// If it's for another server, propagate it on its way.

	if destinationSID == s.Catbox.Config.TS6SID {
		s.GotPONG = true

		if s.Bursting && s.GotPING {
			s.Catbox.noticeOpers(fmt.Sprintf("Burst with %s over.", s.Server.Name))
			s.Bursting = false
		}
		return
	}

	// It's for a different server. Propagate it.

	destinationServer, exists := s.Catbox.Servers[destinationSID]
	if !exists {
		s.quit("Unknown destination server (PONG)")
		return
	}

	if destinationServer.isLocal() {
		destinationServer.LocalServer.maybeQueueMessage(m)
		return
	}
	destinationServer.ClosestServer.maybeQueueMessage(m)
}

func (s *LocalServer) errorCommand(m irc.Message) {
	s.quit("Bye")
}

// UID command introduces a client. It is on the server that is the source.
func (s *LocalServer) uidCommand(m irc.Message) {
	// Parameters: <nick> <hopcount> <nick TS> <umodes> <username> <hostname> <IP> <UID> :<real name>
	// :8ZZ UID will 1 1475024621 +i will blashyrkh. 0 8ZZAAAAAB :will

	// Is this a valid SID (format)?
	if !isValidSID(m.Prefix) {
		s.quit("Invalid SID")
		return
	}
	sid := TS6SID(m.Prefix)

	// Do we know the server the message originates on?
	usersServer, exists := s.Catbox.Servers[TS6SID(sid)]
	if !exists {
		s.quit("Message from unknown server")
		return
	}

	if !isValidUID(m.Params[7]) {
		s.quit("Invalid UID")
		return
	}
	uid := TS6UID(m.Params[7])

	nickTS, err := strconv.ParseInt(m.Params[2], 10, 64)
	if err != nil {
		s.quit("Invalid nick TS")
		return
	}

	// Is this a valid nick?
	if !isValidNick(s.Catbox.Config.MaxNickLength, m.Params[0]) {
		log.Printf("Invalid nick (%s)", m.Params[0])
		s.quit(fmt.Sprintf("Invalid NICK! (%s)", m.Params[0]))
		return
	}
	displayNick := m.Params[0]

	// Is there a nick collision?
	collidedUID, exists := s.Catbox.Nicks[canonicalizeNick(displayNick)]

	// Collision. The TS6 protocol defines more detailed rules. I simply kill the
	// one with the newer Nick TS without caring about user@host. I also tell
	// every server rather than limiting the extent of the KILL message.
	//
	// Note it's possible to have two KILL messages. One generated by us, and one
	// from the other side. We'll see an unknown user message for the second
	// processed.
	if exists {
		collidedUser := s.Catbox.Users[collidedUID]
		if nickTS < collidedUser.NickTS {
			s.Catbox.issueKill(nil, collidedUser, "Nick collision, newer killed")
		} else if nickTS == collidedUser.NickTS {
			s.Catbox.issueKill(nil, collidedUser, "Nick collision, both killed")
			s.Catbox.issueKill(nil, &User{UID: uid}, "Nick collision, both killed")
			return
		} else if nickTS > collidedUser.NickTS {
			s.Catbox.issueKill(nil, &User{UID: uid}, "Nick collision, newer killed")
			return
		}
	}

	hopCount, err := strconv.ParseInt(m.Params[1], 10, 8)
	if err != nil {
		s.quit("Invalid hop count")
		return
	}

	// I get Nick TS above.

	umodes := make(map[byte]struct{})
	for i, umode := range m.Params[3] {
		if i == 0 {
			if umode != '+' {
				s.quit("Malformed umode")
				return
			}
			continue
		}

		// I only support +i and +o right now.
		if umode == 'i' || umode == 'o' {
			umodes[byte(umode)] = struct{}{}
			continue
		}
	}

	username := m.Params[4]
	if !isValidUser(username) {
		s.quit("Invalid username")
		return
	}

	// We could validate hostname
	hostname := m.Params[5]

	// We could validate IP
	ip := m.Params[6]

	// I get UID ahead of time, above.

	if !isValidRealName(m.Params[8]) {
		s.quit("Invalid real name")
		return
	}
	realName := m.Params[8]

	// OK, the user looks good.

	u := &User{
		DisplayNick:   displayNick,
		HopCount:      int(hopCount),
		NickTS:        int64(nickTS),
		Modes:         umodes,
		Username:      username,
		Hostname:      hostname,
		IP:            ip,
		UID:           uid,
		RealName:      realName,
		Channels:      make(map[string]*Channel),
		ClosestServer: s,
		Server:        usersServer,
	}

	if u.isOperator() {
		s.Catbox.Opers[u.UID] = u
	}
	s.Catbox.Nicks[canonicalizeNick(displayNick)] = u.UID
	s.Catbox.Users[u.UID] = u

	// No reply needed I think.

	// Tell our other servers.
	for _, server := range s.Catbox.LocalServers {
		if server == s {
			continue
		}
		server.maybeQueueMessage(m)
	}

	// Tell local operators.
	if !s.Bursting {
		for _, oper := range s.Catbox.Opers {
			if !oper.isLocal() {
				continue
			}
			_, exists := oper.Modes['C']
			if !exists {
				continue
			}
			oper.LocalUser.serverNotice(fmt.Sprintf("CLICONN %s %s %s %s %s (%s)",
				u.DisplayNick, u.Username, u.Hostname, u.IP, u.RealName, u.Server.Name))
		}
	}

	s.Catbox.updateCounters(false)
}

func (s *LocalServer) privmsgCommand(m irc.Message) {
	// Parameters: <msgtarget> <text to be sent>

	if len(m.Params) == 0 {
		// 411 ERR_NORECIPIENT
		s.messageFromServer("411", []string{"No recipient given (PRIVMSG)"})
		return
	}

	if len(m.Params) == 1 {
		// 412 ERR_NOTEXTTOSEND
		s.messageFromServer("412", []string{"No text to send"})
		return
	}

	// Determine the source.
	// We can receive NOTICE from servers.
	// Otherwise it must be a user.
	source := ""
	if m.Command == "NOTICE" {
		sourceServer, exists := s.Catbox.Servers[TS6SID(m.Prefix)]
		if exists {
			source = sourceServer.Name
		}
	}

	// If we don't know source yet, then it must be a user.
	if source == "" {
		sourceUser, exists := s.Catbox.Users[TS6UID(m.Prefix)]
		if exists {
			source = sourceUser.nickUhost()
		}
	}

	if source == "" {
		s.quit(fmt.Sprintf("Unknown source (%s)", m.Command))
	}

	// Is target a user?
	if isValidUID(m.Params[0]) {
		targetUID := TS6UID(m.Params[0])

		targetUser, exists := s.Catbox.Users[targetUID]
		if exists {
			// We either deliver it to a local user, and done, or we need to propagate
			// it to another server.
			if targetUser.isLocal() {
				// Source and target were UIDs. Translate to uhost and nick
				// respectively.
				m.Params[0] = targetUser.DisplayNick
				targetUser.LocalUser.maybeQueueMessage(irc.Message{
					Prefix:  source,
					Command: m.Command,
					Params:  m.Params,
				})
			} else {
				// Propagate to the server we know the target user through.
				targetUser.ClosestServer.maybeQueueMessage(m)
			}

			return
		}

		// Fall through. Treat it as a channel name.
	}

	// See if it's a channel.

	channel, exists := s.Catbox.Channels[canonicalizeChannel(m.Params[0])]
	if !exists {
		log.Printf("PRIVMSG to unknown target %s", m.Params[0])
		return
	}

	// Inform all members of the channel.
	// Message local users directly.
	// If a user is remote, then we record the server to send the message towards.
	toServers := make(map[*LocalServer]struct{})
	for memberUID := range channel.Members {
		member := s.Catbox.Users[memberUID]

		if member.isLocal() {
			member.LocalUser.maybeQueueMessage(irc.Message{
				Prefix:  source,
				Command: m.Command,
				Params:  m.Params,
			})
			continue
		}

		// Remote user. We need to propagate it towards them.
		if member.ClosestServer != s {
			toServers[member.ClosestServer] = struct{}{}
		}
	}

	// Propagate message to any servers that need it.
	for server := range toServers {
		server.maybeQueueMessage(m)
	}
}

// SID tells us about a new server.
func (s *LocalServer) sidCommand(m irc.Message) {
	// Parameters: <server name> <hop count> <SID> <description>
	// e.g.: :8ZZ SID irc3.example.com 2 9ZQ :My Desc
	if len(m.Params) < 4 {
		// 461 ERR_NEEDMOREPARAMS
		s.messageFromServer("461", []string{"SID", "Not enough parameters"})
		return
	}

	// Do I know this origin? (The server it's linked to)
	linkedToServer, exists := s.Catbox.Servers[TS6SID(m.Prefix)]
	if !exists {
		s.quit(fmt.Sprintf("Unknown origin (SID) %s", m.Prefix))
		return
	}

	name := m.Params[0]

	hopCount, err := strconv.ParseInt(m.Params[1], 10, 8)
	if err != nil {
		s.quit(fmt.Sprintf("Invalid hop count: %s", err))
		return
	}

	if !isValidSID(m.Params[2]) {
		s.quit("Invalid SID")
		return
	}
	sid := TS6SID(m.Params[2])

	desc := m.Params[3]

	newServer := &Server{
		SID:           sid,
		Name:          name,
		Description:   desc,
		HopCount:      int(hopCount),
		ClosestServer: s,
		LinkedTo:      linkedToServer,
	}

	s.Catbox.Servers[sid] = newServer

	// Propagate to our connected servers.
	for _, server := range s.Catbox.LocalServers {
		// Don't tell the server we just heard it from.
		if server == s {
			continue
		}

		server.maybeQueueMessage(m)
	}

	// We don't need to tell the new server about the servers we are connected to.
	// They'll be informed by the server they linked to about us.

	s.Catbox.noticeLocalOpers(fmt.Sprintf("%s is introducing server %s",
		s.Server.Name, newServer.Name))
}

// SJOIN occurs in two contexts:
// 1. During bursts to inform us of channels and users in the channels.
// 2. Outside bursts to inform us of channel creation (not joins in general)
func (s *LocalServer) sjoinCommand(m irc.Message) {
	// Parameters: <channel TS> <channel name> <modes> [mode params] :<UIDs>
	// e.g., :8ZZ SJOIN 1475187553 #test2 +sn :@8ZZAAAAAB

	// Do we know this server?
	_, exists := s.Catbox.Servers[TS6SID(m.Prefix)]
	if !exists {
		s.quit("Unknown server")
		return
	}

	if len(m.Params) < 4 {
		// 461 ERR_NEEDMOREPARAMS
		s.messageFromServer("461", []string{"SJOIN", "Not enough parameters"})
		return
	}

	channelTS, err := strconv.ParseInt(m.Params[0], 10, 64)
	if err != nil {
		s.quit(fmt.Sprintf("Invalid channel TS: %s: %s", m.Params[0], err))
		return
	}

	chanName := m.Params[1]

	// Currently I ignore modes. All channels have the same mode, or we pretend so
	// anyway.

	channel, channelExists := s.Catbox.Channels[canonicalizeChannel(chanName)]
	if !channelExists {
		channel = &Channel{
			Name:    canonicalizeChannel(chanName),
			Members: make(map[TS6UID]struct{}),
			TS:      channelTS,
		}
		s.Catbox.Channels[channel.Name] = channel
	}

	// Update channel TS if needed. To oldest.
	if channelTS < channel.TS {
		channel.TS = channelTS
	}

	// If we had mode parameters, then user list is bumped up one.
	userList := m.Params[3]
	if len(m.Params) > 4 {
		userList = m.Params[4]
	}

	// Look at each of the members we were told about.
	uidsRaw := strings.Split(userList, " ")
	for _, uidRaw := range uidsRaw {
		// May have op/voice prefix.
		// Cut it off for now. I currently don't support op/voice.
		uidRaw = strings.TrimLeft(uidRaw, "@+")

		user, exists := s.Catbox.Users[TS6UID(uidRaw)]
		if !exists {
			// We may not know the user in case of nick collision where we killed.
			// them and forgot them. Allow this.
			log.Printf("SJOIN for unknown user %s, ignoring", uidRaw)
			if !channelExists {
				delete(s.Catbox.Channels, channel.Name)
			}
			return
		}

		// We could check if we already have them flagged as in the channel.

		// Flag them as being in the channel.
		channel.Members[user.UID] = struct{}{}
		user.Channels[channel.Name] = channel

		// Tell our local users who are in the channel.
		for memberUID := range channel.Members {
			member := s.Catbox.Users[memberUID]
			if !member.isLocal() {
				continue
			}

			member.LocalUser.maybeQueueMessage(irc.Message{
				Prefix:  user.nickUhost(),
				Command: "JOIN",
				Params:  []string{channel.Name},
			})
		}
	}

	// Propagate.
	for _, server := range s.Catbox.LocalServers {
		// Don't send it to the server we just heard it from.
		if server == s {
			continue
		}

		server.maybeQueueMessage(m)
	}
}

// We receive TB commands during burst if the other side supports the TB
// capability. They tell us about the topic of a channel.
// Check the TS and potentially update the topic, tell local users, and
// propagate.
func (s *LocalServer) tbCommand(m irc.Message) {
	// Parameters: <channel> <topic TS> [topic setter nick!user@host] <topic>
	// Setter is optional.
	if len(m.Params) < 3 {
		// 461 ERR_NEEDMOREPARAMS
		s.messageFromServer("461", []string{"TB", "Not enough parameters"})
		return
	}

	// Look up the server telling us about this.
	server, exists := s.Catbox.Servers[TS6SID(m.Prefix)]
	if !exists {
		s.quit("Unknown server (TB)")
		return
	}

	// Look up the channel. We must know about it already.
	channel, exists := s.Catbox.Channels[canonicalizeChannel(m.Params[0])]
	if !exists {
		s.quit("Unknown channel (TB)")
		return
	}

	topicTS, err := strconv.ParseInt(m.Params[1], 10, 64)
	if err != nil {
		s.quit("Invalid topic TS (TB)")
		return
	}

	// We could validate the format is nick!user@host.
	// Use server name for setter if setter not present.
	setter := ""
	if len(m.Params) >= 4 {
		setter = m.Params[2]
	} else {
		setter = server.Name
	}

	topic := ""
	if len(m.Params) >= 4 {
		topic = m.Params[3]
	} else {
		topic = m.Params[2]
	}

	// If the topic matches what we have, nothing to do.
	if topic == channel.Topic {
		return
	}

	// The topic is different. Should we accept the other side though?
	acceptTopic := false

	// Accept it if we have none set.
	if len(channel.Topic) == 0 {
		acceptTopic = true
	}

	// Accept it if their topic is older.
	if topicTS < channel.TopicTS {
		acceptTopic = true
	}

	if !acceptTopic {
		return
	}

	// We either have no topic, or our topic is set but we're receiving an older
	// one.

	// Update our records.
	channel.Topic = topic
	channel.TopicSetter = setter
	channel.TopicTS = topicTS

	// Tell our local clients about the topic change.
	for memberUID := range channel.Members {
		member := s.Catbox.Users[memberUID]
		if !member.isLocal() {
			continue
		}

		member.LocalUser.maybeQueueMessage(irc.Message{
			Prefix:  server.Name,
			Command: "TOPIC",
			Params:  []string{channel.Name, channel.Topic},
		})
	}

	// Propagate to other servers.
	for _, ls := range s.Catbox.LocalServers {
		if ls == s {
			continue
		}
		ls.maybeQueueMessage(m)
	}
}

func (s *LocalServer) joinCommand(m irc.Message) {
	// Parameters: <channel TS> <channel> +
	// Prefix is UID.

	// No support for JOIN 0.

	if len(m.Params) < 3 {
		// 461 ERR_NEEDMOREPARAMS
		s.messageFromServer("461", []string{"JOIN", "Not enough parameters"})
		return
	}

	// Do we know the user?
	user, exists := s.Catbox.Users[TS6UID(m.Prefix)]
	if !exists {
		s.quit("Unknown UID (JOIN)")
		return
	}

	channelTS, err := strconv.ParseInt(m.Params[0], 10, 64)
	if err != nil {
		s.quit("Invalid TS (JOIN)")
		return
	}

	chanName := canonicalizeChannel(m.Params[1])
	if !isValidChannel(chanName) {
		s.quit("Invalid channel name")
		return
	}

	// Create the channel if necessary.
	channel, exists := s.Catbox.Channels[chanName]
	if !exists {
		channel = &Channel{
			Name:    chanName,
			Members: make(map[TS6UID]struct{}),
			TS:      channelTS,
		}
		s.Catbox.Channels[channel.Name] = channel
	}

	// Update channel TS if needed. To oldest.
	if channelTS < channel.TS {
		channel.TS = channelTS
	}

	// Put the user in it.
	channel.Members[user.UID] = struct{}{}
	user.Channels[channel.Name] = channel

	// Tell our local users who are in the channel.
	for memberUID := range channel.Members {
		member := s.Catbox.Users[memberUID]
		if !member.isLocal() {
			continue
		}

		member.LocalUser.maybeQueueMessage(irc.Message{
			Prefix:  user.nickUhost(),
			Command: "JOIN",
			Params:  []string{channel.Name},
		})
	}

	// Propagate.
	for _, server := range s.Catbox.LocalServers {
		if server == s {
			continue
		}

		server.maybeQueueMessage(m)
	}
}

func (s *LocalServer) nickCommand(m irc.Message) {
	// Parameters: <nick> <nick TS>

	if len(m.Params) < 2 {
		// 461 ERR_NEEDMOREPARAMS
		s.messageFromServer("461", []string{"NICK", "Not enough parameters"})
		return
	}

	// Find the user.
	user, exists := s.Catbox.Users[TS6UID(m.Prefix)]
	if !exists {
		s.quit("Unknown user (NICK)")
		return
	}

	nick := m.Params[0]

	nickTS, err := strconv.ParseInt(m.Params[1], 10, 64)
	if err != nil {
		s.quit("Invalid TS (NICK)")
		return
	}

	// Is there a nick collision?
	collidedUID, exists := s.Catbox.Nicks[canonicalizeNick(nick)]

	// Collision. The TS6 protocol defines more detailed rules. I simply kill the
	// one with the newer Nick TS without caring about user@host. I also tell
	// every server rather than limiting the extent of the KILL message.
	if exists {
		collidedUser := s.Catbox.Users[collidedUID]
		if nickTS < collidedUser.NickTS {
			s.Catbox.issueKill(nil, collidedUser, "Nick collision, newer killed")
		} else if nickTS == collidedUser.NickTS {
			s.Catbox.issueKill(nil, collidedUser, "Nick collision, both killed")
			s.Catbox.issueKill(nil, user, "Nick collision, both killed")
			return
		} else if nickTS > collidedUser.NickTS {
			s.Catbox.issueKill(nil, user, "Nick collision, newer killed")
			return
		}
	}

	// Tell our local clients who are in a channel with this user.
	// Tell each user only once.
	// Do this prior to updating the user record as it needs to come from the
	// old nick!user@host.
	toldUsers := make(map[TS6UID]struct{})
	for _, channel := range user.Channels {
		for memberUID := range channel.Members {
			member := s.Catbox.Users[memberUID]
			if !member.isLocal() {
				continue
			}

			_, exists := toldUsers[member.UID]
			if exists {
				continue
			}
			toldUsers[member.UID] = struct{}{}

			member.LocalUser.maybeQueueMessage(irc.Message{
				Prefix:  user.nickUhost(),
				Command: "NICK",
				Params:  []string{nick},
			})
		}
	}

	// Update their nick and nick TS.
	user.DisplayNick = nick
	user.NickTS = nickTS

	// Propagate to other servers.
	for _, server := range s.Catbox.LocalServers {
		if server == s {
			continue
		}
		server.maybeQueueMessage(m)
	}
}

func (s *LocalServer) partCommand(m irc.Message) {
	// Params: <comma separated list of channels> <message>
	// NOTE: I don't support comma separating channels.

	if len(m.Params) < 1 {
		// 461 ERR_NEEDMOREPARAMS
		s.messageFromServer("461", []string{"PART", "Not enough parameters"})
		return
	}

	chanName := canonicalizeChannel(m.Params[0])

	msg := ""
	if len(m.Params) > 1 {
		msg = m.Params[1]
	}

	// Look up the source user.
	sourceUser, exists := s.Catbox.Users[TS6UID(m.Prefix)]
	if !exists {
		s.quit("Unknown user (PART)")
		return
	}

	// Look up the channel.
	channel, exists := s.Catbox.Channels[chanName]
	if !exists {
		s.quit("Unknown channel (PART)")
		return
	}

	// Remove them from the channel.

	// While we expect these map keys to be set, check just to be safe.

	_, exists = sourceUser.Channels[chanName]
	if exists {
		delete(sourceUser.Channels, chanName)
	}

	_, exists = channel.Members[sourceUser.UID]
	if exists {
		delete(channel.Members, sourceUser.UID)
	}

	if len(channel.Members) == 0 {
		delete(s.Catbox.Channels, channel.Name)
	}

	// Tell local users about the part.
	params := []string{channel.Name}
	if len(msg) > 0 {
		params = append(params, msg)
	}
	for memberUID := range channel.Members {
		member := s.Catbox.Users[memberUID]
		if !member.isLocal() {
			continue
		}

		member.LocalUser.maybeQueueMessage(irc.Message{
			Prefix:  sourceUser.nickUhost(),
			Command: "PART",
			Params:  params,
		})
	}

	// Propagate to all other servers.
	for _, server := range s.Catbox.LocalServers {
		if server == s {
			continue
		}
		server.maybeQueueMessage(m)
	}
}

func (s *LocalServer) wallopsCommand(m irc.Message) {
	// Params: <text to send>
	if len(m.Params) < 1 {
		s.quit("Invalid parameters (WALLOPS)")
		return
	}

	text := m.Params[0]

	// Origin is either a user or a server.

	origin := ""
	user, exists := s.Catbox.Users[TS6UID(m.Prefix)]
	if exists {
		origin = user.nickUhost()
	}
	server, exists := s.Catbox.Servers[TS6SID(m.Prefix)]
	if exists {
		origin = server.Name
	}

	if len(origin) == 0 {
		s.quit("Unknown origin (WALLOPS)")
		return
	}

	// Send WALLOPS to all our local opers.
	for _, oper := range s.Catbox.Opers {
		if !oper.isLocal() {
			continue
		}
		oper.LocalUser.maybeQueueMessage(irc.Message{
			Prefix:  origin,
			Command: "WALLOPS",
			Params:  []string{text},
		})
	}

	// Propagate to other servers.
	for _, ls := range s.Catbox.LocalServers {
		if ls == s {
			continue
		}
		ls.maybeQueueMessage(m)
	}
}

// QUIT tells us a remote client is gone.
func (s *LocalServer) quitCommand(m irc.Message) {
	// Parameters: <quit comment>

	// Origin is the user who quit.
	user, exists := s.Catbox.Users[TS6UID(m.Prefix)]
	if !exists {
		s.quit("Unknown user (QUIT)")
		return
	}

	message := ""
	if len(m.Params) >= 1 {
		message = m.Params[0]
	}

	s.Catbox.quitRemoteUser(user, message)

	// Propagate to all servers.
	for _, server := range s.Catbox.LocalServers {
		if server == s {
			continue
		}
		server.maybeQueueMessage(m)
	}
}

// MODE tells us about either channel or user changes.
// Right now I don't really support channel modes, so ignore those all together.
// For user modes, I track only +i and +o. Ignore the rest.
func (s *LocalServer) modeCommand(m irc.Message) {
	// User mode message parameters: <client UID> <umode changes>
	if len(m.Params) < 2 {
		return
	}

	// Look up the user making the change.
	user, exists := s.Catbox.Users[TS6UID(m.Prefix)]
	if !exists {
		s.quit("Unknown prefix (MODE)")
		return
	}

	// The first parameter is the target. It's the user's UID or a channel name.
	user2, exists := s.Catbox.Users[TS6UID(m.Params[0])]
	if !exists {
		// Assume it is a channel.
		return
	}

	// It must be the same as the prefix.
	if user != user2 {
		s.quit("Invalid MODE: User changing another's mode")
		return
	}

	// Now look at the mode changes that took place.
	// Default to + like we do with user MODE command.
	motion := '+'
	for _, c := range m.Params[1] {
		if c == '+' || c == '-' {
			motion = c
			continue
		}

		// I only track +i and +o right now.
		if c == 'i' || c == 'o' {
			if motion == '+' {
				user.Modes[byte(c)] = struct{}{}
				if c == 'o' {
					s.Catbox.Opers[user.UID] = user
					s.Catbox.noticeLocalOpers(fmt.Sprintf("%s@%s became an operator.",
						user.DisplayNick, user.Server.Name))
				}
			} else {
				_, exists := user.Modes[byte(c)]
				if exists {
					delete(user.Modes, byte(c))
					if c == 'o' {
						delete(s.Catbox.Opers, user.UID)
					}
				}
			}
		}
	}

	// We don't need to tell local clients anything. Only the user who changed
	// needs to know, and they are remote, so their server told them.

	// Propagate.
	for _, server := range s.Catbox.LocalServers {
		if server == s {
			continue
		}
		server.maybeQueueMessage(m)
	}
}

func (s *LocalServer) topicCommand(m irc.Message) {
	// Parameters: <channel> [topic]
	if len(m.Params) < 1 {
		// 461 ERR_NEEDMOREPARAMS
		s.messageFromServer("461", []string{"TOPIC", "Not enough parameters"})
		return
	}

	// Find source user.
	sourceUser, exists := s.Catbox.Users[TS6UID(m.Prefix)]
	if !exists {
		s.quit("Unknown source user (TOPIC)")
		return
	}

	chanName := canonicalizeChannel(m.Params[0])
	channel, exists := s.Catbox.Channels[chanName]
	if !exists {
		// 403 ERR_NOSUCHCHANNEL
		s.messageFromServer("403", []string{chanName, "No such channel"})
		return
	}

	// We could check the source user has ops (when we support ops).
	// We could check the source is on the channel.

	// Tell local clients who are in the channel about the topic change.
	params := []string{channel.Name}
	if len(m.Params) >= 2 && len(m.Params[1]) > 0 {
		params = append(params, m.Params[1])
	}
	for memberUID := range channel.Members {
		member := s.Catbox.Users[memberUID]
		if !member.isLocal() {
			continue
		}
		member.LocalUser.maybeQueueMessage(irc.Message{
			Prefix:  sourceUser.nickUhost(),
			Command: "TOPIC",
			Params:  params,
		})
	}

	// Propagate to other servers.
	for _, server := range s.Catbox.LocalServers {
		if server == s {
			continue
		}
		server.maybeQueueMessage(m)
	}
}

// SQUIT tells us there is a server departing.
// This can tell us either a server remote from us is going, or that the local
// server is delinking. In practice, if the local server is delinking we will
// be told with an ERROR message, and not see any SQUIT.
func (s *LocalServer) squitCommand(m irc.Message) {
	// Parameters: <target server> <comment>
	if len(m.Params) < 2 {
		// 461 ERR_NEEDMOREPARAMS
		s.messageFromServer("461", []string{"SQUIT", "Not enough parameters"})
		return
	}

	targetServer, exists := s.Catbox.Servers[TS6SID(m.Params[0])]
	if !exists {
		s.quit("Unknown server (SQUIT)")
		return
	}

	// This should NOT be the local server, or any local server for that matter.
	// When we're splitting from a local server, we'll be told with an ERROR
	// command.
	//
	// It could be if an operator on another server wants us to split, but I
	// choose to not support it.
	for _, server := range s.Catbox.LocalServers {
		if server.Server == targetServer {
			s.quit("I won't SQUIT a local server")
			return
		}
	}

	// It's remote. We need to forget it and tell all local users about relevant
	// things (split, etc).
	s.serverSplitCleanUp(targetServer)

	// Propagate to other servers.
	for _, server := range s.Catbox.LocalServers {
		if server == s {
			continue
		}
		server.maybeQueueMessage(m)
	}

	reason := ""
	if len(m.Params) > 1 {
		reason = m.Params[1]
	}

	from := ""
	if targetServer.LinkedTo != nil {
		from = targetServer.LinkedTo.Name
	}

	s.Catbox.noticeLocalOpers(fmt.Sprintf("Server %s delinked from %s: %s",
		s.Server.Name, from, reason))
}

// KILL tells us about a client getting disconnected forcefully.
// The user may be local or remote. Either way, we need to propagate the KILL
// everywhere.
func (s *LocalServer) killCommand(m irc.Message) {
	// Parameters: <target user UID> <reason>
	// Reason has format:
	// <source> (<reason text>)
	// Where <source> looks something like:
	// <killer server name>!<killer user host>!<killer user username>!<killer nick>
	if len(m.Params) < 2 {
		// 461 ERR_NEEDMOREPARAMS
		s.messageFromServer("461", []string{"KILL", "Not enough parameters"})
		return
	}

	// It's not guaranteed this is a user. It may be a server.
	source := ""
	sourceUser, exists := s.Catbox.Users[TS6UID(m.Prefix)]
	if exists {
		source = sourceUser.DisplayNick
	}
	if len(source) == 0 {
		sourceServer, exists := s.Catbox.Servers[TS6SID(m.Prefix)]
		if exists {
			source = sourceServer.Name
		}
	}

	targetUser, exists := s.Catbox.Users[TS6UID(m.Params[0])]
	if !exists {
		s.Catbox.noticeOpers(fmt.Sprintf("Received KILL for unknown user %s",
			m.Params[0]))
		return
	}

	// Pull out the source info and the reason.
	sourceAndReason := m.Params[1]

	space := strings.Index(sourceAndReason, " ")
	if space == -1 {
		s.quit("Malformed kill reason")
		return
	}
	sourceInfo := sourceAndReason[:space]

	sourceAndReason = sourceAndReason[space:]

	lparen := strings.Index(sourceAndReason, "(")
	rparen := strings.LastIndex(sourceAndReason, ")")
	if lparen == -1 || rparen == -1 || lparen > rparen ||
		lparen+1 == len(sourceAndReason) {
		s.quit("Malformed KILL reason")
		return
	}
	reason := sourceAndReason[lparen+1 : rparen]

	// Tell our local opers about this.
	s.Catbox.noticeLocalOpers(
		fmt.Sprintf("Received KILL message for %s. From %s Path: %s (%s)",
			targetUser.DisplayNick, source, sourceInfo, reason))

	quitReason := fmt.Sprintf("Killed (%s (%s))", source, reason)

	// If it's a local user, kick it off.
	if targetUser.isLocal() {
		s.Catbox.noticeOpers(fmt.Sprintf("Killing local user %s",
			targetUser.DisplayNick))
		targetUser.LocalUser.quit(quitReason, false)
	}

	// If it's remote, we need to forget about this user.
	if targetUser.isRemote() {
		// Remove the user from each channel.
		// Also, tell each local client that is in 1+ channel with the user that
		// this user quit.
		s.Catbox.quitRemoteUser(targetUser, quitReason)
	}

	// Propagate to all servers.
	for _, server := range s.Catbox.LocalServers {
		if server == s {
			continue
		}
		server.maybeQueueMessage(m)
	}
}

// For the ENCAP command spec, see:
// http://www.leeh.co.uk/ircd/encap.txt
//
// Essentially it is a way to propagate commands to all servers. Apparently it
// was to work around issues where commands were not propagating correctly.
//
// In practice, some commands propagate this way, such as KLINE. We see KLINE
// propagated from servers in the TS6 protocol in this manner:
// :1SNAAAAAF ENCAP * KLINE 0 * 127.5.5.5 :bye bye
//
// Format:
// :<source, UID or possibly SID?> ENCAP <destination> <subcommand>
// [params for the subcommand]
//
// Destination can be a mask. For servers it may be a wildcard. For clients
// apparently not.
//
// Currently I will assume destination mask is always *.
//
// If the encapsulated command is one I know about, operate on it locally.
func (s *LocalServer) encapCommand(m irc.Message) {
	if len(m.Params) < 2 {
		// 461 ERR_NEEDMOREPARAMS
		s.messageFromServer("461", []string{"ENCAP", "Not enough parameters"})
		return
	}

	// I don't look at destination right now. Assume it's for this server too.

	// Extract the sub command and its parameters.
	subCommand := strings.ToUpper(m.Params[1])
	subParams := []string{}
	if len(m.Params) > 2 {
		subParams = append(subParams, m.Params[2:]...)
	}

	// Do we want to do something with the encapsulated command?
	if subCommand == "KLINE" {
		s.klineCommand(irc.Message{
			Prefix:  m.Prefix,
			Command: subCommand,
			Params:  subParams,
		})
	}
	if subCommand == "UNKLINE" {
		s.unklineCommand(irc.Message{
			Prefix:  m.Prefix,
			Command: subCommand,
			Params:  subParams,
		})
	}
	if subCommand == "GCAP" {
		s.gcapCommand(irc.Message{
			Prefix:  m.Prefix,
			Command: subCommand,
			Params:  subParams,
		})
	}

	// Propagate everywhere.
	for _, server := range s.Catbox.LocalServers {
		if server == s {
			continue
		}
		server.maybeQueueMessage(m)
	}
}

// The KLINE command comes only in ENCAP messages.
//
// Apply a ban on user@host.
//
// Currently this is persistent only for the runtime.
//
// Parameters: <duration> <user mask> <host mask> [<reason>]
// Example (with ENCAP portion dropped):
// :1SNAAAAAF KLINE 0 * 127.5.5.5 :bye bye
//
// At this time we treat all KLINEs as "permanent" for the duration of our run.
// i.e., we ignore duration.
func (s *LocalServer) klineCommand(m irc.Message) {
	if len(m.Params) < 3 {
		// 461 ERR_NEEDMOREPARAMS
		s.messageFromServer("461", []string{"KLINE", "Not enough parameters"})
		return
	}

	source := ""
	user, exists := s.Catbox.Users[TS6UID(m.Prefix)]
	if exists {
		source = user.DisplayNick
	}
	if source == "" {
		// I'm unsure if we can get klines this way (servers as source).
		server, exists := s.Catbox.Servers[TS6SID(m.Prefix)]
		if exists {
			source = server.Name
		}
	}
	if source == "" {
		log.Printf("Unknown source for KLINE command")
		return
	}

	// I ignore duration at this time. It's permanent.

	reason := "<No reason given>"
	if len(m.Params) > 3 {
		reason = m.Params[3]
	}

	kline := KLine{
		UserMask: m.Params[1],
		HostMask: m.Params[2],
		Reason:   reason,
	}

	s.Catbox.addAndApplyKLine(kline, source, reason)

	// We don't need to propagate. Since KLINE comes in through an ENCAP command,
	// it was propagated there.
}

// UNKLINE <user mask> <host mask>
func (s *LocalServer) unklineCommand(m irc.Message) {
	if len(m.Params) < 2 {
		// 461 ERR_NEEDMOREPARAMS
		s.messageFromServer("461", []string{"UNKLINE", "Not enough parameters"})
		return
	}

	source := ""
	user, exists := s.Catbox.Users[TS6UID(m.Prefix)]
	if exists {
		source = user.DisplayNick
	}
	if source == "" {
		// I'm unsure if we can get klines this way (servers as source).
		server, exists := s.Catbox.Servers[TS6SID(m.Prefix)]
		if exists {
			source = server.Name
		}
	}
	if source == "" {
		log.Printf("Unknown source for UNKLINE command")
		return
	}

	userMask := m.Params[0]
	hostMask := m.Params[1]

	// Find it.
	s.Catbox.removeKLine(userMask, hostMask, source)

	// We don't need to propagate as UNKLINE comes inside ENCAP.
}

// Upon link to a server, it tells us about the capabilities of all servers
// it introduces to us. This comes in this form:
// :3SN ENCAP * GCAP :QS EX CHW IE GLN KNOCK TB ENCAP SAVE SAVETS_100
// Where 3SN is the server with these capabilities.
// We remember this information so we can tell servers we link to in the future.
func (s *LocalServer) gcapCommand(m irc.Message) {
	if len(m.Params) == 0 {
		// We're TS6 only. Servers must have at least QS and ENCAP to be TS6.
		s.quit(fmt.Sprintf("GCAP from %s with no capabs", m.Prefix))
		return
	}

	// Ensure we know the server.
	server, exists := s.Catbox.Servers[TS6SID(m.Prefix)]
	if !exists {
		s.quit(fmt.Sprintf("Unknown server (GCAP): %s", m.Prefix))
		return
	}

	capabs := parseCapabsString(m.Params[0])

	// For TS6 we must have QS and ENCAP.

	_, exists = capabs["QS"]
	if !exists {
		s.quit(fmt.Sprintf("%s is missing capab QS", server.Name))
		return
	}

	_, exists = capabs["ENCAP"]
	if !exists {
		s.quit(fmt.Sprintf("%s is missing capab ENCAP", server.Name))
		return
	}

	if server.Capabs != nil {
		s.quit(fmt.Sprintf("Already received GCAP from %s!", server.Name))
		return
	}

	server.Capabs = capabs

	// We don't need to propagate. GCAP comes inside ENCAP. Already propagated.
}

// Params: <uid> <nick>
// e.g. :1SNAAAAAB WHOIS 000AAAAAA :horgh
func (s *LocalServer) whoisCommand(m irc.Message) {
	if len(m.Params) < 2 {
		// 461 ERR_NEEDMOREPARAMS
		s.messageFromServer("461", []string{"WHOIS", "Not enough parameters"})
		return
	}

	sourceUser, exists := s.Catbox.Users[TS6UID(m.Prefix)]
	if !exists {
		log.Printf("WHOIS from unknown user %s", m.Prefix)
		return
	}

	user, exists := s.Catbox.Users[TS6UID(m.Params[0])]
	if !exists {
		// 401 ERR_NOSUCHNICK
		sourceUser.ClosestServer.maybeQueueMessage(irc.Message{
			Prefix:  s.Catbox.Config.ServerName,
			Command: "401",
			Params: []string{sourceUser.DisplayNick, m.Params[0],
				"No such nick/channel"},
		})
	}

	// If it's a local user, reply back to the server.
	if user.isLocal() {
		msgs := s.Catbox.createWHOISResponse(user, sourceUser, true)
		for _, msg := range msgs {
			sourceUser.ClosestServer.maybeQueueMessage(msg)
		}
		return
	}

	// If remote user, propagate to the closest server
	user.ClosestServer.maybeQueueMessage(m)
}

// We've got a numeric command.
// For example, a reply to a remote WHOIS.
//
// Look up where it's going and if it's local, send it to the local client.
// If it's remote, propagate it on.
func (s *LocalServer) numericCommand(m irc.Message) {
	// Only servers should be sending numerics.
	sourceServer, exists := s.Catbox.Servers[TS6SID(m.Prefix)]
	if !exists {
		log.Printf("Numeric from unknown server %s", m.Prefix)
		return
	}

	if len(m.Params) == 0 {
		log.Printf("Numeric with no parameters")
		return
	}

	// Find the target.
	user, exists := s.Catbox.Users[TS6UID(m.Params[0])]
	if !exists {
		log.Printf("Numeric %s for unknown user %s", m.Command, m.Params[0])
		return
	}

	// If it's for a local client, then send it to them, and done.
	if user.isLocal() {
		// First parameter is the target user. We get it as UID. Turn into NICK.
		params := []string{user.DisplayNick}
		if len(m.Params) > 1 {
			params = append(params, m.Params[1:]...)
		}
		user.LocalUser.maybeQueueMessage(irc.Message{
			Prefix:  sourceServer.Name,
			Command: m.Command,
			Params:  params,
		})
		return
	}

	// It's destined somewhere remote. Pass it on its way.
	user.ClosestServer.maybeQueueMessage(m)
}

// This is a custom command I built into ratbox.
// For more information, refer to where I generate it in registerUser().
// Do nothing but propagate.
func (s *LocalServer) cliconnCommand(m irc.Message) {
	for _, server := range s.Catbox.LocalServers {
		if server == s {
			continue
		}
		server.maybeQueueMessage(m)
	}
}

// A user is either set AWAY or set UNAWAY.
// Parameters: [away reason]
func (s *LocalServer) awayCommand(m irc.Message) {
	reason := ""
	if len(m.Params) > 0 {
		reason = m.Params[0]
	}

	// Find the user.
	user, exists := s.Catbox.Users[TS6UID(m.Prefix)]
	if !exists {
		s.quit("Unknown user (AWAY)")
		return
	}

	// Flag the user away or not.

	if len(reason) > 0 {
		user.AwayMessage = reason
	} else {
		// If they're not away and this is telling us to set unaway, just ignore it.
		// Something is wrong.
		if len(user.AwayMessage) == 0 {
			return
		}
		user.AwayMessage = ""
	}

	// Propagate.
	for _, server := range s.Catbox.LocalServers {
		if server == s {
			continue
		}
		server.maybeQueueMessage(m)
	}
}

// An INVITE command.
// Source: <user UID>
// Parameters: <target user UID> <channel> [channel TS]
// Apparently channel TS is optional, but deprecated to be. However ratbox does
// not send it, so allow it to not be present.
// If channel TS indicates newer, ignore it.
// Propagate it towards the target user if it's not a local user. Otherwise
// if it's a local user, tell the user.
func (s *LocalServer) inviteCommand(m irc.Message) {
	if len(m.Params) < 2 {
		// 461 ERR_NEEDMOREPARAMS
		s.messageFromServer("461", []string{"INVITE", "Not enough parameters"})
		return
	}

	// Find source user.
	sourceUser, exists := s.Catbox.Users[TS6UID(m.Prefix)]
	if !exists {
		s.quit("Unknown source user (INVITE)")
		return
	}

	// Find the target user.
	targetUser, exists := s.Catbox.Users[TS6UID(m.Params[0])]
	if !exists {
		s.quit("Unknown target user (INVITE)")
		return
	}

	// Find the channel.
	channel, exists := s.Catbox.Channels[canonicalizeChannel(m.Params[1])]
	if !exists {
		s.quit("Unknown channel (INVITE)")
		return
	}

	// Assume the other side checked the invite was valid to proceed.

	// Check channel TS if it's given.
	if len(m.Params) >= 3 {
		channelTS, err := strconv.ParseInt(m.Params[3], 10, 64)
		if err != nil {
			s.quit(fmt.Sprintf("Invalid channel TS: %s: %s", m.Params[0], err))
			return
		}

		// If channel TS indicates the channel is newer than what we know, ignore.
		if channelTS > channel.TS {
			s.Catbox.noticeOpers(fmt.Sprintf("INVITE from %s to %s for %s has newer TS",
				sourceUser.DisplayNick, targetUser.DisplayNick, channel.Name))
			return
		}
	}

	// If it's a local user, tell the user, and that's it.
	if targetUser.isLocal() {
		targetUser.LocalUser.maybeQueueMessage(irc.Message{
			Prefix:  sourceUser.nickUhost(),
			Command: "INVITE",
			Params:  []string{targetUser.DisplayNick, channel.Name},
		})
		return
	}

	// If it's a remote user, propagate the message on its way.
	targetUser.ClosestServer.maybeQueueMessage(m)
}
