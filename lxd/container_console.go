package main

import (
	"encoding/json"
	"fmt"
	"io/ioutil"
	"net/http"
	"os"
	"strconv"
	"sync"

	"github.com/gorilla/mux"
	"github.com/gorilla/websocket"

	"github.com/lxc/lxd/shared"
	"github.com/lxc/lxd/shared/api"
	"github.com/lxc/lxd/shared/logger"
)

type consoleWs struct {
	// container currently worked on
	container container

	// uid to chown pty to
	rootUid int64

	// gid to chown pty to
	rootGid int64

	// websocket connections to bridge pty fds to
	conns map[int]*websocket.Conn

	// locks needed to access the "conns" member
	connsLock sync.Mutex

	// channel to wait until all websockets are properly connected
	allConnected chan bool

	// channel to wait until the control socket is connected
	controlConnected chan bool

	// map file descriptors -> secret
	fds map[int]string

	// terminal width
	width int

	// terminal height
	height int
}

func (s *consoleWs) Metadata() interface{} {
	fds := shared.Jmap{}
	for fd, secret := range s.fds {
		if fd == -1 {
			fds["control"] = secret
		} else {
			fds[strconv.Itoa(fd)] = secret
		}
	}

	return shared.Jmap{"fds": fds}
}

func (s *consoleWs) Connect(op *operation, r *http.Request, w http.ResponseWriter) error {
	secret := r.FormValue("secret")
	if secret == "" {
		return fmt.Errorf("missing secret")
	}

	for fd, fdSecret := range s.fds {
		if secret == fdSecret {
			conn, err := shared.WebsocketUpgrader.Upgrade(w, r, nil)
			if err != nil {
				return err
			}

			s.connsLock.Lock()
			s.conns[fd] = conn
			s.connsLock.Unlock()

			if fd == -1 {
				s.controlConnected <- true
				return nil
			}

			s.connsLock.Lock()
			for i, c := range s.conns {
				if i != -1 && c == nil {
					s.connsLock.Unlock()
					return nil
				}
			}
			s.connsLock.Unlock()

			s.allConnected <- true
			return nil
		}
	}

	/* If we didn't find the right secret, the user provided a bad one,
	 * which 403, not 404, since this operation actually exists */
	return os.ErrPermission
}

func (s *consoleWs) Do(op *operation) error {
	<-s.allConnected

	var err error
	master := &os.File{}
	slave := &os.File{}
	master, slave, err = shared.OpenPty(s.rootUid, s.rootGid)
	if err != nil {
		return err
	}

	if s.width > 0 && s.height > 0 {
		shared.SetSize(int(master.Fd()), s.width, s.height)
	}

	controlExit := make(chan bool)
	var wgEOF sync.WaitGroup

	wgEOF.Add(1)
	go func() {
		select {
		case <-s.controlConnected:
			break

		case <-controlExit:
			return
		}

		for {
			s.connsLock.Lock()
			conn := s.conns[-1]
			s.connsLock.Unlock()

			mt, r, err := conn.NextReader()
			if mt == websocket.CloseMessage {
				break
			}

			if err != nil {
				logger.Debugf("Got error getting next reader %s", err)
				break
			}

			buf, err := ioutil.ReadAll(r)
			if err != nil {
				logger.Debugf("Failed to read message %s", err)
				break
			}

			command := api.ContainerConsoleControl{}

			err = json.Unmarshal(buf, &command)
			if err != nil {
				logger.Debugf("Failed to unmarshal control socket command: %s", err)
				continue
			}

			if command.Command == "window-resize" {
				winchWidth, err := strconv.Atoi(command.Args["width"])
				if err != nil {
					logger.Debugf("Unable to extract window width: %s", err)
					continue
				}

				winchHeight, err := strconv.Atoi(command.Args["height"])
				if err != nil {
					logger.Debugf("Unable to extract window height: %s", err)
					continue
				}

				err = shared.SetSize(int(master.Fd()), winchWidth, winchHeight)
				if err != nil {
					logger.Debugf("Failed to set window size to: %dx%d", winchWidth, winchHeight)
					continue
				}

				logger.Debugf("Set window size to: %dx%d", winchWidth, winchHeight)
			}
		}
	}()

	go func() {
		s.connsLock.Lock()
		conn := s.conns[0]
		s.connsLock.Unlock()

		logger.Debugf("Starting to mirror websocket")
		readDone, writeDone := shared.WebsocketConsoleMirror(conn, master, master)

		<-readDone
		<-writeDone
		logger.Debugf("Finished to mirror websocket")

		conn.Close()
		wgEOF.Done()
	}()

	finisher := func(cmdErr error) error {
		slave.Close()

		s.connsLock.Lock()
		conn := s.conns[-1]
		s.connsLock.Unlock()

		if conn == nil {
			controlExit <- true
		}

		wgEOF.Wait()

		master.Close()

		return cmdErr
	}

	err = s.container.Console(slave)
	if err != nil {
		return err
	}

	return finisher(err)
}

func containerConsolePost(d *Daemon, r *http.Request) Response {
	name := mux.Vars(r)["name"]
	c, err := containerLoadByName(d.State(), name)
	if err != nil {
		return SmartError(err)
	}

	err = fmt.Errorf("Container is not running")
	if !c.IsRunning() {
		return BadRequest(err)
	}

	err = fmt.Errorf("Container is frozen")
	if c.IsFrozen() {
		return BadRequest(err)
	}

	post := api.ContainerConsolePost{}
	buf, err := ioutil.ReadAll(r.Body)
	if err != nil {
		return BadRequest(err)
	}

	err = json.Unmarshal(buf, &post)
	if err != nil {
		return BadRequest(err)
	}

	ws := &consoleWs{}
	ws.fds = map[int]string{}

	idmapset, err := c.IdmapSet()
	if err != nil {
		return InternalError(err)
	}

	if idmapset != nil {
		ws.rootUid, ws.rootGid = idmapset.ShiftIntoNs(0, 0)
	}

	ws.conns = map[int]*websocket.Conn{}
	ws.conns[-1] = nil
	ws.conns[0] = nil
	for i := -1; i < len(ws.conns)-1; i++ {
		ws.fds[i], err = shared.RandomCryptoString()
		if err != nil {
			return InternalError(err)
		}
	}

	ws.allConnected = make(chan bool, 1)
	ws.controlConnected = make(chan bool, 1)

	ws.container = c
	ws.width = post.Width
	ws.height = post.Height

	resources := map[string][]string{}
	resources["containers"] = []string{ws.container.Name()}

	op, err := operationCreate(operationClassWebsocket, resources,
		ws.Metadata(), ws.Do, nil, ws.Connect)
	if err != nil {
		return InternalError(err)
	}

	return OperationResponse(op)
}
