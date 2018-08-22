package websocketrpc

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"reflect"
	"strings"
	"time"

	"github.com/gorilla/websocket"
)

const (
	sendQueueSize = 5
)

// Server represents the websocketrpc server
type Server struct {
	sendChan   chan interface{}
	addr       string
	logging    bool
	serviceMap map[string]*service
	semaCh     chan struct{}
}

// NewServer creates a new WebSocketRPCServer on addr (ex. localhost:8080)
// Errors are always logged; setting logging adds additional logging of activity
// Designed for only one concurrent connection to frontend
func NewServer(addr string, logging bool) *Server {
	srv := &Server{addr: addr, logging: logging}
	srv.serviceMap = make(map[string]*service)
	srv.semaCh = make(chan struct{}, 1)                  // Max of 1 concurrent connection
	srv.sendChan = make(chan interface{}, sendQueueSize) // Max Queue of 5 messages
	return srv
}

// Register type with the server
// Methods must be Exported (capital first letter)
// First Arg must be the Server type
// Optional second Arg for data
// If have return type, it is encoded and sent back over connection
func (srv *Server) Register(rcvr interface{}) error {
	s := new(service)
	s.typ = reflect.TypeOf(rcvr)
	s.rcvr = reflect.ValueOf(rcvr)
	sname := reflect.Indirect(s.rcvr).Type().Name()
	if sname == "" {
		return fmt.Errorf("WebSocketRPC: Failed to register %v", sname)
	}
	s.name = sname
	s.method = suitableMethods(s.typ)
	if len(s.method) == 0 {
		return fmt.Errorf("WebSocketRPC: Did not find any methods of %v to register", sname)
	}
	srv.serviceMap[sname] = s
	return nil
}

// StartServer starts listening and serving RPC
func (srv *Server) StartServer() {
	http.HandleFunc("/", srv.rpcHandler)
	log.Fatal(http.ListenAndServe(srv.addr, nil))
}

// Send a message with subject and data over websocket in JSON format
// Safe to use from multiple goroutines as it is consolidated over a channel
func (srv *Server) Send(subject string, data interface{}) {
	if data != nil {
		srv.sendChan <- Message{Subject: subject, Data: data}
	} else {
		srv.sendChan <- struct {
			Subject string
		}{Subject: subject}
	}
}

func (srv *Server) rpcHandler(w http.ResponseWriter, r *http.Request) {
	// Make sure only one connection
	timeOut := time.NewTimer(time.Millisecond * 1200)
	select {
	case srv.semaCh <- struct{}{}:
		defer func() {
			<-srv.semaCh
		}()
	case <-timeOut.C:
		log.Println("WebSocketRPC: Attempt to open second connection, rejecting")
		http.Error(w, "WebSocket connection already in use", 429)
		return
	}
	// Upgrade to websocket connection
	upgrader := websocket.Upgrader{} // use default options
	con, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Println(err)
		return
	}
	if srv.logging {
		log.Println("WebSocketRPC: Started websocket connection with ", con.RemoteAddr())
	}
	// Setup send channel on this connection
	quitCh := make(chan struct{})
	go srv.handleSendChannel(con, quitCh) // Launch goroutine to manage all sends as can only have one send thread
	defer func() {
		quitCh <- struct{}{}
	}()
	// Process incoming messages
	for {
		messageType, p, err := con.ReadMessage()
		if err != nil {
			log.Println(err)
			return
		}
		if messageType == 1 {
			srv.processRPCMessage(con, p)
		} else {
			log.Printf("WebSocketRPC: Received Message Type %v, not handling", messageType)
		}
	}
}

func (srv *Server) handleSendChannel(con *websocket.Conn, quitCh chan struct{}) {
	for {
		select {
		case item := <-srv.sendChan:
			err := con.WriteJSON(item)
			if err != nil {
				log.Println("WebSocketRPC: Error sending return ", err)
			} else if srv.logging {
				log.Println("WebSocketRPC: Sent ", item)
			}
		case <-quitCh:
			return
		}
	}
}

func (srv *Server) processRPCMessage(con *websocket.Conn, b []byte) {
	type SocketRPCItem struct {
		Subject string
		Data    json.RawMessage
	}
	var sock SocketRPCItem
	err := json.Unmarshal(b, &sock)
	if err != nil || sock.Subject == "" {
		log.Println("WebSocketRPC: Received bad message: ", string(b))
		return
	}
	parts := strings.Split(sock.Subject, ".")
	// parts[0] = Type, [1] = Method
	if len(parts) != 2 {
		log.Println("WebSocketRPC: Bad Subject Format: ", sock.Subject)
		return
	}
	service, found := srv.serviceMap[parts[0]]
	if found == false {
		log.Println("WebSocketRPC: Could not find type: ", parts[0])
		return
	}
	mt, found := service.method[parts[1]]
	if found == false {
		log.Printf("WebSocketRPC: In Type %v Could not find method: %v", parts[0], parts[1])
		return
	}
	function := mt.method.Func
	var returnValues []reflect.Value
	switch mt.hasArg {
	case false:
		if srv.logging {
			log.Printf("WebSocketRPC: RPC call %v", sock.Subject)
		}
		returnValues = function.Call([]reflect.Value{service.rcvr, reflect.ValueOf(srv)})
	case true:
		argv, err := buildArg(mt, sock.Data)
		if err != nil {
			log.Printf("WebSocketRPC: For method %v, data sent did not match method signature: %v", sock.Subject, string(sock.Data))
			return
		}
		if srv.logging {
			log.Printf("WebSocketRPC: RPC call %v with: %v", sock.Subject, string(sock.Data))
		}
		returnValues = function.Call([]reflect.Value{service.rcvr, reflect.ValueOf(srv), argv})
	}
	if len(returnValues) > 0 {
		srv.sendChan <- returnValues[0].Interface()
	}
}

// buildArg returns the argument for the function call
func buildArg(mt *methodType, d json.RawMessage) (reflect.Value, error) {
	var argv reflect.Value
	argIsValue := false // if true, need to indirect before calling.
	if mt.ArgType.Kind() == reflect.Ptr {
		argv = reflect.New(mt.ArgType.Elem())
	} else {
		argv = reflect.New(mt.ArgType)
		argIsValue = true
	}
	err := json.Unmarshal(d, argv.Interface())
	if err != nil {
		return reflect.Value{}, err
	}
	if argIsValue {
		argv = argv.Elem()
	}
	return argv, nil
}

// Message represents the standard structure for sending messages over the socket using this API
type Message struct {
	Subject string
	Data    interface{}
}

// service represents the info on a registered type
type service struct {
	name   string                 // name of service
	rcvr   reflect.Value          // receiver of methods for the service
	typ    reflect.Type           // type of the receiver
	method map[string]*methodType // registered methods
}

type methodType struct {
	method  reflect.Method
	hasArg  bool
	ArgType reflect.Type
}

// suitableMethods returns suitable methods from the type being registered
func suitableMethods(typ reflect.Type) map[string]*methodType {
	methods := make(map[string]*methodType)
	for m := 0; m < typ.NumMethod(); m++ {
		method := typ.Method(m)
		mtype := method.Type
		mname := method.Name
		// Method must be exported, otherwise skip
		if method.PkgPath != "" {
			log.Printf("WebSocketRPC: Register method %q is not exported, skipping", mname)
			continue
		}
		savedMethod := &methodType{method: method}
		// NumIn(0) = receiver, (1) should be Server, (2) should be optional data
		switch mtype.NumIn() {
		case 2:
			savedMethod.hasArg = false
		case 3:
			savedMethod.hasArg = true
			savedMethod.ArgType = mtype.In(2)
		default:
			log.Printf("WebSocketRPC: Register method %q has bad number of arguments", mname)
		}
		if mtype.NumOut() > 1 {
			log.Printf("WebSocketRPC: Register method %q has bad number of return values, max is 1", mname)
		}
		methods[mname] = savedMethod
	}
	return methods
}
