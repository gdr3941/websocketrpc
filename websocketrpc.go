package websocketrpc

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"reflect"
	"strings"

	"github.com/gorilla/websocket"
)

// Server manages the RPC over websocket
type Server struct {
	addr       string
	serviceMap map[string]*service
	logging    bool
}

// NewServer creates a new WebSocketRPCServer on addr (ex. localhost:8080)
// errors are always logged; setting logging adds additional logging of calls
func NewServer(addr string, logging bool) *Server {
	srv := &Server{addr: addr, logging: logging}
	srv.serviceMap = make(map[string]*service)
	return srv
}

// Register a type with the server
// Methods must be Exported (capital first letter)
// First Arg must be the websocket.Conn type for receiving the active connection
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
func Send(conn *websocket.Conn, subject string, data interface{}) error {
	if conn == nil {
		return fmt.Errorf("WebSocketRPC: No connection; send failed")
	}
	type SocketRPCItem struct {
		Subject string
		Data    interface{}
	}
	t := &SocketRPCItem{Subject: subject, Data: data}
	return conn.WriteJSON(t)
}

func (srv *Server) rpcHandler(w http.ResponseWriter, r *http.Request) {
	upgrader := websocket.Upgrader{} // use default options
	con, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Println(err)
		return
	}
	if srv.logging {
		log.Println("WebSocketRPC: Started websocket connection with ", con.RemoteAddr())
	}
	for {
		messageType, p, err := con.ReadMessage()
		if err != nil {
			log.Println(err)
			return
		}
		if messageType == 1 {
			srv.processRPCMessage(con, p)
		} else {
			log.Printf("WebSocketRPC: Received Message Type %v, not handling\n", messageType)
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
		log.Printf("WebSocketRPC: In Type %v Could not find method: %v\n", parts[0], parts[1])
		return
	}

	function := mt.method.Func
	var returnValues []reflect.Value
	switch mt.hasArg {
	case false:
		if srv.logging {
			log.Printf("WebSocketRPC: RPC call %v", sock.Subject)
		}
		returnValues = function.Call([]reflect.Value{service.rcvr, reflect.ValueOf(con)})
	case true:
		argv, err := buildArg(mt, sock.Data)
		if err != nil {
			log.Printf("WebSocketRPC: For method %v, data sent did not match method signature: %v\n", sock.Subject, string(sock.Data))
			return
		}
		if srv.logging {
			log.Printf("WebSocketRPC: RPC call %v with: %v\n ", sock.Subject, string(sock.Data))
		}
		returnValues = function.Call([]reflect.Value{service.rcvr, reflect.ValueOf(con), argv})
	}
	if len(returnValues) > 0 {
		err = con.WriteJSON(returnValues[0].Interface())
		if err != nil {
			log.Println("WebSocketRPC: Error sending return ", err)
		}
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

// service represents the info on one class type (name) that is registered
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
			log.Printf("WebSocketRPC: Register method %q is not exported, skipping\n", mname)
			continue
		}
		savedMethod := &methodType{method: method}
		// NumIn(0) = receiver, (1) should be websocket.Conn, (2) should be optional data
		switch mtype.NumIn() {
		case 2:
			savedMethod.hasArg = false
		case 3:
			savedMethod.hasArg = true
			savedMethod.ArgType = mtype.In(2)
		default:
			log.Printf("WebSocketRPC: Register method %q has bad number of arguments\n", mname)
		}
		if mtype.NumOut() > 1 {
			log.Printf("WebSocketRPC: Register method %q has bad number of return values, max is 1\n", mname)
		}
		methods[mname] = savedMethod
	}
	return methods
}
