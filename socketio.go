package socketio

import (
	"encoding/json"
	"log"
	"net/http"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

const (
	pingInterval = 30 * time.Second
	pingTimeout  = 60 * time.Second
	pongTimeout  = 5 * time.Second
)

type SocketIOClient struct {
	conn          *websocket.Conn
	sendQueue     chan []byte
	disconnect    chan struct{}
	lastPing      time.Time
	pingTimer     *time.Timer
	ackHandlers   map[int]func(interface{})
	disconnectMtx sync.Mutex
	server        *SocketIOServer
	room          *Room
}

type SocketIOHandler func(c *SocketIOClient, data interface{})

type SocketIOServer struct {
	upgrader *websocket.Upgrader
	clients  map[*websocket.Conn]*SocketIOClient
	handlers map[string]SocketIOHandler
	lock     sync.Mutex
	rooms    map[string]*Room
}

type Room struct {
	name      string
	clients   map[*SocketIOClient]bool
	join      chan *SocketIOClient
	leave     chan *SocketIOClient
	broadcast chan []byte
	stop      chan struct{}
}

func NewSocketIOServer() *SocketIOServer {
	upgrader := &websocket.Upgrader{
		CheckOrigin: func(r *http.Request) bool {
			// WebSocket bağlantısının güvenliği hakkında gerektiğinde özelleştirmeler yapabilirsiniz
			return true
		},
	}

	return &SocketIOServer{
		upgrader: upgrader,
		clients:  make(map[*websocket.Conn]*SocketIOClient),
		handlers: make(map[string]SocketIOHandler),
		rooms:    make(map[string]*Room),
	}
}

func (s *SocketIOServer) Use(event string, handler SocketIOHandler) {
	s.handlers[event] = handler
}

func (s *SocketIOServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	conn, err := s.upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Printf("Hata: %v", err)
		return
	}

	client := NewSocketIOClient(conn)
	client.server = s
	s.addClient(client)
	client.Start()

	defer s.removeClient(conn)
}

func (s *SocketIOServer) addClient(client *SocketIOClient) {
	s.lock.Lock()
	defer s.lock.Unlock()

	s.clients[client.conn] = client
}

func (s *SocketIOServer) removeClient(conn *websocket.Conn) {
	s.lock.Lock()
	defer s.lock.Unlock()

	client, ok := s.clients[conn]
	if ok {
		client.disconnectClient()
		delete(s.clients, conn)
	}
}

func (s *SocketIOServer) createRoom(name string) *Room {
	s.lock.Lock()
	defer s.lock.Unlock()

	room := &Room{
		name:      name,
		clients:   make(map[*SocketIOClient]bool),
		join:      make(chan *SocketIOClient),
		leave:     make(chan *SocketIOClient),
		broadcast: make(chan []byte),
		stop:      make(chan struct{}),
	}

	s.rooms[name] = room

	go room.start()

	return room
}

func (s *SocketIOServer) RemoveRoom(name string) {
	s.lock.Lock()
	defer s.lock.Unlock()

	if room, ok := s.rooms[name]; ok {
		delete(s.rooms, name)
		close(room.stop)
	}
}

func (s *SocketIOServer) BroadcastMessage(event string, message interface{}) {
	s.lock.Lock()
	defer s.lock.Unlock()

	packet, err := createPacket(event, message)
	if err != nil {
		log.Printf("Hata: %v", err)
		return
	}

	for _, client := range s.clients {
		client.sendQueue <- packet
	}
}

func createPacket(event string, message interface{}) ([]byte, error) {
	data := []interface{}{event, message}
	packet, err := json.Marshal(data)
	if err != nil {
		return nil, err
	}

	packetType := "42" // Socket.IO packet type for an event
	packet = append([]byte(packetType), packet...)

	return packet, nil
}

func NewSocketIOClient(conn *websocket.Conn) *SocketIOClient {
	return &SocketIOClient{
		conn:        conn,
		sendQueue:   make(chan []byte, 256),
		disconnect:  make(chan struct{}),
		lastPing:    time.Now(),
		ackHandlers: make(map[int]func(interface{})),
	}
}

func (c *SocketIOClient) Start() {
	c.pingTimer = time.AfterFunc(pingInterval, c.sendPing)
	defer c.disconnectClient()

	go c.readLoop()
	go c.writeLoop()

	for {
		select {
		case <-c.disconnect:
			return
		case message := <-c.sendQueue:
			err := c.conn.WriteMessage(websocket.TextMessage, message)
			if err != nil {
				log.Printf("Hata: %v", err)
				return
			}
		}
	}
}

func (c *SocketIOClient) sendPing() {
	c.pingTimer.Reset(pingInterval)

	if time.Since(c.lastPing) > pingTimeout {
		log.Printf("Ping zaman aşımı, istemciyi bağlantıyı sonlandırılıyor.")
		c.conn.Close()
		return
	}

	err := c.conn.WriteMessage(websocket.PingMessage, nil)
	if err != nil {
		log.Printf("Hata: %v", err)
		return
	}
}

func (c *SocketIOClient) readLoop() {
	c.conn.SetReadLimit(1024)
	c.conn.SetReadDeadline(time.Now().Add(pongTimeout))
	c.conn.SetPongHandler(func(string) error {
		c.lastPing = time.Now()
		c.conn.SetReadDeadline(time.Now().Add(pongTimeout))
		return nil
	})

	for {
		_, message, err := c.conn.ReadMessage()
		if err != nil {
			log.Printf("Hata: %v", err)
			c.disconnectClient()
			return
		}

		packetType := string(message[:2])
		if packetType == "2" { // Socket.IO packet type for an event
			c.handleEventPacket(message[2:])
		} else if packetType == "3" { // Socket.IO packet type for a ping
			c.handlePingPacket()
		}
	}
}

func (c *SocketIOClient) writeLoop() {
	for {
		select {
		case <-c.disconnect:
			return
		case message := <-c.sendQueue:
			err := c.conn.WriteMessage(websocket.TextMessage, message)
			if err != nil {
				log.Printf("Hata: %v", err)
				c.disconnectClient()
				return
			}
		}
	}
}

func (c *SocketIOClient) disconnectClient() {
	c.disconnectMtx.Lock()
	defer c.disconnectMtx.Unlock()

	select {
	case <-c.disconnect:
		return
	default:
		close(c.disconnect)
		c.conn.Close()
	}
}

func (c *SocketIOClient) handleEventPacket(data []byte) {
	var eventPacket []json.RawMessage
	err := json.Unmarshal(data, &eventPacket)
	if err != nil {
		log.Printf("Hata: %v", err)
		return
	}

	event := string(eventPacket[0])
	handler, ok := c.server.handlers[event]
	if !ok {
		log.Printf("Hata: Tanımsız olay %s", event)
		return
	}

	var eventData interface{}
	err = json.Unmarshal(eventPacket[1], &eventData)
	if err != nil {
		log.Printf("Hata: %v", err)
		return
	}

	handler(c, eventData)
}

func (c *SocketIOClient) handlePingPacket() {
	err := c.conn.WriteMessage(websocket.PongMessage, nil)
	if err != nil {
		log.Printf("Hata: %v", err)
		return
	}
}

func (c *SocketIOClient) Emit(event string, data interface{}) {
	packet, err := createPacket(event, data)
	if err != nil {
		log.Printf("Hata: %v", err)
		return
	}

	c.sendQueue <- packet
}

func (c *SocketIOClient) JoinRoom(roomName string) {
	c.server.lock.Lock()
	defer c.server.lock.Unlock()

	room, ok := c.server.rooms[roomName]
	if !ok {
		room = c.server.createRoom(roomName)
	}

	c.room = room
	room.join <- c
}

func (c *SocketIOClient) LeaveRoom() {
	if c.room != nil {
		c.room.leave <- c
		c.room = nil
	}
}

func (r *Room) start() {
	for {
		select {
		case client := <-r.join:
			r.clients[client] = true
			client.room = r
		case client := <-r.leave:
			delete(r.clients, client)
			client.room = nil
		case message := <-r.broadcast:
			for client := range r.clients {
				client.sendQueue <- message
			}
		case <-r.stop:
			for client := range r.clients {
				delete(r.clients, client)
				client.room = nil
			}
			return
		}
	}
}
