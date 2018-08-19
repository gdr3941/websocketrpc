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
}

// NewServer creates a new WebSocketRPCServer on addr (ex. localhost:8080)
func NewServer(addr string) *Server {
	srv := &Server{}
	srv.addr = addr
	srv.serviceMap = make(map[string]*service)
	return srv
}

// Register a type with the RPC server
func (srv *Server) Register(rcvr interface{}) error {
	s := new(service)
	s.typ = reflect.TypeOf(rcvr)
	s.rcvr = reflect.ValueOf(rcvr)
	sname := reflect.Indirect(s.rcvr).Type().Name()
	if sname == "" {
		return fmt.Errorf("Failed to register %v", sname)
	}
	s.name = sname
	s.method = suitableMethods(s.typ)
	if len(s.method) == 0 {
		return fmt.Errorf("Did not find any methods of %v to register", sname)
	}
	srv.serviceMap[sname] = s
	return nil
}

// StartServer starts listening and serving RPC
func (srv *Server) StartServer() {
	http.HandleFunc("/", srv.rpcHandler)
	log.Fatal(http.ListenAndServe(srv.addr, nil))
}

func (srv *Server) rpcHandler(w http.ResponseWriter, r *http.Request) {
	upgrader := websocket.Upgrader{} // use default options
	con, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Println(err)
		return
	}
	for {
		messageType, p, err := con.ReadMessage()
		if err != nil {
			log.Println(err)
			return
		}
		if messageType == 1 {
			//log.Printf("Message: %v\n", string(p))
			srv.processRPCMessage(con, p)
		} else {
			log.Printf("Received Message Type %v, not handling\n", messageType)
		}
	}
}

func (srv *Server) processRPCMessage(conn *websocket.Conn, b []byte) {
	type SocketRPCItem struct {
		Subject string
		Data    json.RawMessage
	}
	var sock SocketRPCItem
	err := json.Unmarshal(b, &sock)
	if err != nil || sock.Subject == "" {
		log.Println("Received bad message: ", string(b))
		return
	}
	parts := strings.Split(sock.Subject, ".")
	// parts[0] = Type, [1] = Method
	if len(parts) != 2 {
		log.Println("Bad Subject Format: ", sock.Subject)
		return
	}
	service, found := srv.serviceMap[parts[0]]
	if found == false {
		log.Println("Could not find type: ", parts[0])
		return
	}
	mt, found := service.method[parts[1]]
	if found == false {
		log.Printf("In Type %v Could not find method: %v\n", parts[0], parts[1])
		return
	}

	function := mt.method.Func
	var returnValues []reflect.Value

	if mt.hasArg == false {
		log.Printf("RPC call %v", sock.Subject)
		returnValues = function.Call([]reflect.Value{service.rcvr})
	} else {
		var argv reflect.Value
		argIsValue := false // if true, need to indirect before calling.
		if mt.ArgType.Kind() == reflect.Ptr {
			argv = reflect.New(mt.ArgType.Elem())
		} else {
			argv = reflect.New(mt.ArgType)
			argIsValue = true
		}
		err = json.Unmarshal(sock.Data, argv.Interface())
		if err != nil {
			log.Printf("For method %v, data sent did not match method signature: %v\n", sock.Subject, string(sock.Data))
			return
		}
		if argIsValue {
			argv = argv.Elem()
		}
		log.Printf("RPC call %v with: %v\n ", sock.Subject, string(sock.Data))
		returnValues = function.Call([]reflect.Value{service.rcvr, argv})
	}
	if len(returnValues) > 0 {
		err = conn.WriteJSON(returnValues[0].Interface())
		if err != nil {
			log.Println("Error sending return ", err)
		}
	}
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
	fmt.Println("Type ", typ)
	fmt.Println("Number of methods to register ", typ.NumMethod())
	for m := 0; m < typ.NumMethod(); m++ {
		method := typ.Method(m)
		mtype := method.Type
		mname := method.Name
		// Method must be exported, otherwise skip
		if method.PkgPath != "" {
			log.Printf("Register: method %q is not exported, skipping\n", mname)
			continue
		}
		savedMethod := &methodType{method: method}
		switch mtype.NumIn() {
		case 1:
			savedMethod.hasArg = false
		case 2:
			savedMethod.hasArg = true
			savedMethod.ArgType = mtype.In(1)
		default:
			log.Printf("Register: method %q has bad number of arguments\n", mname)
		}

		if mtype.NumOut() > 1 {
			log.Printf("Register: method %q has bad number of return values, max is 1\n", mname)
		}
		methods[mname] = savedMethod
	}
	return methods
}
