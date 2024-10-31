package main

import (
	"fmt"
	"github.com/lonng/nano/serialize/json"
	"golang.org/x/sync/singleflight"
	"log"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/lonng/nano"
	"github.com/lonng/nano/component"
	"github.com/lonng/nano/pipeline"
	"github.com/lonng/nano/scheduler"
	"github.com/lonng/nano/session"
)

type (
	Room struct {
		group    *nano.Group
		mu       sync.RWMutex
		userPush map[int64]*DrawPush //用户待推送数据
		dataMu   sync.RWMutex
		drawData map[int]string   //起始绘画框数据
		drawList []map[int]string //记录绘画数据
		listMu   sync.RWMutex
		tempMu   sync.RWMutex
		tempData map[int]string //分时绘画数据
	}

	// RoomManager represents a component that contains a bundle of room
	RoomManager struct {
		component.Base
		timer *scheduler.Timer
		rooms map[int]*Room
		sfg   singleflight.Group
	}

	// UserMessage represents a message that user sent
	UserMessage struct {
		Name    string `json:"name"`
		Content string `json:"content"`
	}

	// NewUser message will be received when new user join room
	NewUser struct {
		Content string `json:"content"`
	}

	// AllMembers contains all members uid
	AllMembers struct {
		Members []int64 `json:"members"`
	}

	// JoinResponse represents the result of joining room
	JoinResponse struct {
		Code   int    `json:"code"`
		Result string `json:"result"`
	}

	stats struct {
		component.Base
		timer         *scheduler.Timer
		outboundBytes int
		inboundBytes  int
	}

	Draw struct {
		X     int    `json:"x"`
		Y     int    `json:"y"`
		Color string `json:"color"`
	}

	DrawPush struct {
		Sy *sync.Mutex
		Ch chan map[int]string
		//SyncData []map[int]string `json:"sync_data"` //用于断线重连时同步数据
		//PushData []map[int]string `json:"push_data"` //同步正常推送数据
	}
)

func (stats *stats) outbound(s *session.Session, msg *pipeline.Message) error {
	stats.outboundBytes += len(msg.Data)
	return nil
}

func (stats *stats) inbound(s *session.Session, msg *pipeline.Message) error {
	stats.inboundBytes += len(msg.Data)
	return nil
}

func (stats *stats) AfterInit() {
	stats.timer = scheduler.NewTimer(time.Second, func() {
		println("OutboundBytes", stats.outboundBytes)
		println("InboundBytes", stats.outboundBytes)
	})
}

const (
	testRoomID = 1
	roomIDKey  = "ROOM_ID"
)

func NewRoomManager() *RoomManager {
	return &RoomManager{
		rooms: map[int]*Room{},
	}
}

// AfterInit component lifetime callback
func (mgr *RoomManager) AfterInit() {
	session.Lifetime.OnClosed(func(s *session.Session) {
		if !s.HasKey(roomIDKey) {
			return
		}
		room := s.Value(roomIDKey).(*Room)
		room.group.Leave(s)
		room.mu.RLock()
		defer room.mu.RUnlock()
		if room.userPush[s.ID()] != nil {
			delete(room.userPush, s.ID())
		}
	})
	mgr.timer = scheduler.NewTimer(time.Minute, func() {
		for roomId, room := range mgr.rooms {
			println(fmt.Sprintf("UserCount: RoomID=%d, Time=%s, Count=%d",
				roomId, time.Now().String(), room.group.Count()))
		}
	})
}

// Join room
func (mgr *RoomManager) Join(s *session.Session, msg []byte) error {
	// NOTE: join test room only in demo
	room, found := mgr.rooms[testRoomID]
	if !found {
		mgr.sfg.Do("room-create", func() (interface{}, error) {
			room = &Room{
				group:    nano.NewGroup(fmt.Sprintf("room-%d", testRoomID)),
				userPush: make(map[int64]*DrawPush),
				drawData: make(map[int]string),
				tempData: make(map[int]string),
			}
			//初始化画框数据
			for i := 0; i < 200; i++ {
				for j := 0; j < 160; j++ {
					room.drawData[i*200+j] = "#FFFFFF"
				}
			}
			mgr.rooms[testRoomID] = room
			go room.PushDrawData()
			go room.ClearDrawList()
			return nil, nil
		})
	}

	fakeUID := s.ID() //just use s.ID as uid !!!
	s.Bind(fakeUID)   // binding session uids.Set(roomIDKey, room)
	s.Set(roomIDKey, room)
	//s.Push("onMembers", &AllMembers{Members: room.group.Members()})
	// notify others
	//room.group.Broadcast("onNewUser", &NewUser{Content: fmt.Sprintf("New user: %d", s.ID())})
	// new user join group
	room.group.Add(s) // add session to group
	room.mu.RLock()
	defer room.mu.RUnlock()
	var drawPush = &DrawPush{Sy: &sync.Mutex{}, Ch: make(chan map[int]string, 100)}
	room.userPush[s.ID()] = drawPush
	//监听推送数据
	drawPush.Sy.Lock()
	//初始化之后解锁
	go func() {
		defer drawPush.Sy.Unlock()
		drawPush.Ch <- room.drawData
		for _, v := range room.drawList {
			drawPush.Ch <- v
		}
	}()
	go func() {
		for {
			select {
			case data := <-drawPush.Ch:
				s.Push("draw", data)
			}
			time.Sleep(time.Millisecond * 1)
		}
	}()

	return s.Response(&JoinResponse{Result: "success"})
}

func (r *Room) PushDrawData() {
	for {
		select {
		case <-time.NewTicker(time.Millisecond * 50).C:
			//每隔50毫秒绘制一次
			r.tempMu.Lock()
			if len(r.tempData) > 0 {
				var wg sync.WaitGroup
				tempData := r.tempData
				for _, v := range r.userPush {
					wg.Add(1)
					go func() {
						v.Sy.Lock()
						v.Ch <- tempData
						v.Sy.Unlock()
					}()
				}
				r.listMu.Lock()
				r.drawList = append(r.drawList, tempData)
				r.listMu.Unlock()
				wg.Done()
				r.tempData = make(map[int]string)
			}
			r.tempMu.Unlock()
		}
	}
}

func (r *Room) ClearDrawList() {
	for {
		select {
		case <-time.NewTicker(time.Second * 10).C:
			//每隔半小时清空一半的数据
			if len(r.drawList) <= 1 {
				break
			}
			tempList := r.drawList
			tempDraw := r.drawData
			for _, v := range tempList[:len(tempList)/2] {
				for kk, vv := range v {
					tempDraw[kk] = vv
				}
			}
			r.listMu.Lock()
			r.drawList = r.drawList[len(tempList)/2:]
			r.drawData = tempDraw
			r.listMu.Unlock()
		}
	}
}

// Message sync last message to all members
func (mgr *RoomManager) Message(s *session.Session, msg *UserMessage) error {
	if !s.HasKey(roomIDKey) {
		return fmt.Errorf("not join room yet")
	}
	room := s.Value(roomIDKey).(*Room)
	return room.group.Broadcast("onMessage", msg)
}

func (mgr *RoomManager) Draw(s *session.Session, draw *Draw) error {
	if !s.HasKey(roomIDKey) {
		return fmt.Errorf("not join room yet")
	}
	room := s.Value(roomIDKey).(*Room)
	//判断是否在画框内
	if draw.X >= 200 || draw.X < 0 || draw.Y >= 160 || draw.Y < 0 {
		return fmt.Errorf("点不在画框内")
	}
	//判断是否为gba
	if !isHexColorCode(draw.Color) {
		return fmt.Errorf("传入颜色不对")
	}
	//推送入对应帧数据
	room.tempMu.Lock()
	defer room.tempMu.Unlock()
	room.tempData[draw.X*200+draw.Y] = draw.Color
	return nil
}

func isHexColorCode(s string) bool {
	// 匹配 #RRGGBB 或 #RRGGBBAA 格式的正则表达式
	re := regexp.MustCompile(`^#([A-Fa-f0-9]{6}|[A-Fa-f0-9]{8})$`)
	return re.MatchString(s)
}

func main() {
	components := &component.Components{}
	components.Register(
		NewRoomManager(),
		component.WithName("room"), // rewrite component and handler name
		component.WithNameFunc(strings.ToLower),
	)

	// traffic stats
	pip := pipeline.New()
	var stats = &stats{}
	pip.Outbound().PushBack(stats.outbound)
	pip.Inbound().PushBack(stats.inbound)

	log.SetFlags(log.LstdFlags | log.Llongfile)
	http.Handle("/web/", http.StripPrefix("/web/", http.FileServer(http.Dir("web"))))
	nano.Listen(":3250",
		nano.WithIsWebsocket(true),
		nano.WithPipeline(pip),
		nano.WithCheckOriginFunc(func(_ *http.Request) bool { return true }),
		nano.WithWSPath("/nano"),
		nano.WithDebugMode(),
		nano.WithSerializer(json.NewSerializer()), // override default serializer
		nano.WithComponents(components),
	)
}
