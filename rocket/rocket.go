package rocket

import (
    "os"
    "fmt"
    "io/ioutil"
    "time"
    "encoding/json"
    "net/http"
    "crypto/sha256"
    "errors"
    "log"

    "github.com/gorilla/websocket"
    "gopkg.in/yaml.v2"
)

type rocketCon struct {
    UserId          string
    UserName        string          `yaml:"user"`
    Password        string          `yaml:"password"`
    AuthToken       string          `yaml:"authtoken"`
    HostName        string          `yaml:"domain"`
    HostSSL         bool            `yaml:"ssl"`
    HostPort        uint16          `yaml:"port"`
    session         string
    channels        map[string] string
    send            chan interface{}
    receive         chan interface{}
    results         map[string] chan map[string] interface{}
    resultsAppend   chan struct {
        string string
        channel chan map[string] interface{}
    }
    resultsDel      chan string
    nextId          chan string
    messages        chan message
    newMessages     chan message
    quit            chan struct{}
}

const STATUS_ONLINE string = "online"
const STATUS_BUSY string = "busy"
const STATUS_AWAY string = "away"
const STATUS_OFFLINE string = "offline"

func NewConnection(username string, password string) (rocketCon, error) {
    var rock rocketCon
    rock.UserName = username
    rock.Password = password
    rock.init()
    return rock, nil
}

func NewConnectionAuthToken(authtoken string) (rocketCon, error) {
    var rock rocketCon
    rock.AuthToken = authtoken
    rock.init()
    return rock, nil
}

func NewConnectionConfig(filename string) (rocketCon, error) {
    var rock rocketCon
    _, err := os.Stat(filename)
    if err != nil {
        return rock, err
    }

    source, err := ioutil.ReadFile(filename)
    if err != nil {
        return rock, err
    }

    rock.HostSSL = true

    err = yaml.Unmarshal(source, &rock)
    if err != nil {
        return rock, err
    }

    if rock.HostName == "" {
        return rock, errors.New("HostName not set")
    }
    if rock.AuthToken == "" && (rock.UserName == "" || rock.Password == "" ){
        return rock, errors.New("AuthToken not set")
    }

    if rock.HostPort == 0 {
        if rock.HostSSL {
            rock.HostPort = 443
        } else {
            rock.HostPort = 80
        }
    }

    err = rock.init()
    return rock, err
}

func (rock *rocketCon) init() (error) {
    // Init variables
    rock.send = make(chan interface{}, 1024)
    rock.receive = make(chan interface{}, 1024)
    rock.resultsAppend = make(chan struct {
        string string
        channel chan map[string] interface{}
    },0)
    rock.resultsDel = make(chan string,1024)
    rock.results = make(map[string] chan map[string] interface{})
    rock.nextId = make(chan string,0)
    rock.messages = make(chan message,1024)
    rock.newMessages = make(chan message,1024)
    rock.quit = make(chan struct{},0)
    rock.channels = make(map[string]string)

    go rock.run()

    // Send Init Messages
    rock.connect()
    err := rock.login()
    if err != nil {
        close(rock.quit)
        return err
    }

    if rock.UserName == "" {
        rock.UserName = rock.RequestUserName(rock.UserId)
    }

    rock.subscribeRooms()
    log.Println("Initialized successfully")
    return nil
}

func (rock *rocketCon) run() {
    // Set some websocket tunables
    const socketreadsizelimit = 65536
    const pingtime = 120 * time.Second
    const timeout = 125 * time.Second

    // Define Websocket URL
    var wsURL string
    if rock.HostSSL {
        wsURL = fmt.Sprintf("wss://%s:%d/websocket", rock.HostName, rock.HostPort)
    } else {
        wsURL = fmt.Sprintf("ws://%s:%d/websocket", rock.HostName, rock.HostPort)
    }

    // Init websocket
    ws, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
    if err != nil {
        fmt.Println(err)
        return
    }
    defer ws.Close()

    // Configure Websocket using Tunables
    ws.SetReadLimit(socketreadsizelimit)
    ws.SetReadDeadline(time.Now().Add(timeout))
    ws.SetPongHandler(func(string) (error) {
        ws.SetReadDeadline(time.Now().Add(timeout))
        return nil
    })
    tick := time.NewTicker(pingtime)
    defer tick.Stop()

    // Manage Method/Subscription Ids
    go func() {
        for i := uint64(0); ; i++{
            i++
            rock.nextId <- fmt.Sprintf("%d",i)
        }
    }()

    // Manage Results map
    go func() {
        for {
            select {
            case addition := <- rock.resultsAppend:
                rock.results[addition.string] = addition.channel
            case remove := <- rock.resultsDel:
                delete(rock.results, remove)
            }
        }
    }()

    // Send Thread
    go func() {
        for {
            msg := <-rock.send
            packet, err := json.Marshal(msg)
            err = ws.WriteMessage(websocket.TextMessage, packet)
            if err != nil {
                fmt.Println("Ping Ticker:", err)
                return
            }
        }
    }()

    // Read Thread
    for {
        _, raw, err := ws.ReadMessage()
        ws.SetReadDeadline(time.Now().Add(timeout))

        if err != nil {
            fmt.Printf("error: %v\n", err)
            break
        }

        var pack map[string] interface{}
        err = json.Unmarshal(raw, &pack)
        if err != nil {
            fmt.Printf("error: %v\n", err)
            continue
        }

        if msg, ok := pack["msg"]; ok {
            switch msg {
            case "connected":
                rock.session = pack["session"].(string)
            case "result":
                if channel, ok := rock.results[pack["id"].(string)]; ok {
                    channel <- pack
                }
                rock.resultsDel <- pack["id"].(string)
            case "added":
                switch pack["collection"].(string) {
                case "users":
                    break
                default:
                    fmt.Println(pack)
                }
            case "updated":
                break
            case "changed":
                obj := pack["fields"].(map[string]interface{})["args"].([]interface{})
                switch pack["collection"].(string) {
                case "stream-notify-user":
                    switch obj[0].(string) {
                    case "inserted":
                        rock.subscribeRoom(obj[1].(map[string]interface{})["rid"].(string))
                    }
                case "stream-room-messages":
                    for _, val := range obj {
                        message := rock.handleMessageObject(val.(map[string]interface{}))
                        if message.IsNew && !message.IsMe {
                            select {
                            case rock.newMessages <- message:
                                break
                            default:
                            }
                        } else {
                            select {
                            case rock.messages <- message:
                                break
                            default:
                            }
                        }
                    }
                }
            case "ready":
                break
            case "ping":
                pong := map[string] string {
                    "msg": "pong",
                }
                rock.send <- pong
            default:
                fmt.Println(string(raw))
            }
        }
    }
    close(rock.quit)
}

func (rock *rocketCon) generateId() string {
    return <-rock.nextId
}

func (rock *rocketCon) watchResults(str string) chan map[string] interface{} {
    c := make(chan map[string] interface{})
    rock.resultsAppend <- struct {
        string string
        channel chan map[string] interface{}
    } {string: str, channel: c}
    return c
}

func (rock *rocketCon) subscribeRoom(rid string) {
    subscribeRoom := map[string] interface{}{
        "msg": "sub",
        "id": rock.generateId(),
        "name": "stream-room-messages",
        "params": []interface{} {
            rid,
            false,
        },
    }
    rock.send <- subscribeRoom
}

func (rock *rocketCon) subscribeRooms() (error) {
    if rock.UserId == "" {
        return errors.New("error: Can't subscribe to rooms if user is not known")
    }
    subscriptionMonitor := map[string] interface{}{
        "msg": "sub",
        "id": rock.generateId(),
        "name": "stream-notify-user",
        "params": []interface{} {
            rock.UserId+"/subscriptions-changed",
            false,
        },
    }
    rock.send <- subscriptionMonitor

    subscriptionsGet := map[string] interface{} {
        "method": "subscriptions/get",
        "params": []map[string] interface{} {
            map[string] interface{} {
                "$date": time.Now().Unix(),
            },
        },
    }
    reply, err := rock.runMethod(subscriptionsGet)
    if err != nil {
        return err
    }

    objects := reply["result"].(map[string] interface{})["update"].([]interface{})

    for index, _ := range objects {
        rock.subscribeRoom(objects[index].(map[string]interface{})["rid"].(string))
        if _, ok := objects[index].(map[string]interface{})["name"]; ok {
            name := objects[index].(map[string]interface{})["name"].(string)
            id := objects[index].(map[string]interface{})["rid"].(string)
            rock.channels[id] = name
        }
    }
    return nil
}

func (rock *rocketCon) restRequest(str string) []byte{
    // Define Websocket URL
    var httpURL string
    if rock.HostSSL {
        httpURL = fmt.Sprintf("https://%s:%d%s", rock.HostName, rock.HostPort, str)
    } else {
        httpURL = fmt.Sprintf("http://%s:%d%s", rock.HostName, rock.HostPort, str)
    }
    // Build Request
    client := &http.Client{}
    request, _ := http.NewRequest("GET", httpURL, nil)
    request.Header.Set("X-Auth-Token", rock.AuthToken)
    request.Header.Set("X-User-Id", rock.UserId)

    // Get Request
    response, _ := client.Do(request)

    // Parse Request
    defer response.Body.Close()
    body, _ := ioutil.ReadAll(response.Body)
    return body
}

func (rock *rocketCon) runMethod(i map[string] interface{}) (map[string] interface{}, error) {
    id := rock.generateId()
    i["msg"] = "method"
    i["id"] = id
    c := rock.watchResults(id)
    defer close(c)
    rock.send <- i
    reply := <- c
    if _, ok := reply["error"]; ok {
        if _, ok := reply["error"].(map[string] interface{})["error"]; ok {
            errNo := reply["error"].(map[string] interface{})["error"].(string)
            errType := reply["error"].(map[string] interface{})["errorType"].(string)
            return reply, errors.New(fmt.Sprintf("Login: %s %s", errNo, errType))
        } else {
            return reply, errors.New("Unknown error")
        }
    }
    return reply, nil
}

func (rock *rocketCon) connect() {
    init := map[string] interface{} {
        "msg": "connect",
        "version": "1",
        "support": []string{"1", "pre2", "pre1"},
    }
    rock.send <- init
}

func (rock *rocketCon) login() error {
    var obj map[string] interface{}
    if rock.AuthToken == "" {
        passhash := fmt.Sprintf("%x",sha256.Sum256([]byte(rock.Password)))
        fmt.Println("Trying "+passhash)
        obj = map[string] interface{} {
            "method": "login",
            "params": []map[string] interface{} {
                map[string] interface{} {
                    "user": map[string] interface {} {
                        "username": rock.UserName,
                    },
                    "password": map[string] interface{} {
                        "digest": passhash,
                        "algorithm": "sha-256",
                    },
                },
            },
        }
    } else {
        obj = map[string] interface{} {
            "method": "login",
            "params": []map[string] interface{} {
                map[string] interface{} {
                    "resume": rock.AuthToken,
                },
            },
        }
    }

    reply, err := rock.runMethod(obj)
    if err != nil {
        return err
    }
    rock.UserId = reply["result"].(map[string] interface{})["id"].(string)
    rock.AuthToken = reply["result"].(map[string] interface{})["token"].(string)
    return nil
}

func (rock *rocketCon) GetMessage() (message, error) {
    var msg message
    select {
    case msg := <- rock.messages:
        return msg, nil
    case msg := <- rock.newMessages:
        return msg, nil
    case <-rock.quit:
        return msg, errors.New("The rocket connection has been closed")
    }
}

func (rock *rocketCon) GetNewMessage() (message, error) {
    var msg message
    select {
    case msg := <- rock.newMessages:
        return msg, nil
    case <-rock.quit:
        return msg, errors.New("The rocket connection has been closed")
    }
}

func (rock *rocketCon) RequestUserName(userid string) string {
    res := rock.restRequest("/api/v1/users.info?userId="+userid)
    var m map[string] interface{}
    err := json.Unmarshal(res, &m)
    if err != nil {
        fmt.Println(err)
        return ""
    }
    return m["user"].(map[string]interface{})["name"].(string)
}

func (rock *rocketCon) RefreshChannelCache() (error) {
    obj := map[string] interface{} {
        "method": "rooms/get",
    }

    reply, err := rock.runMethod(obj)
    if err != nil {
        return err
    }
    for _, val := range reply["result"].([]interface{}) {
        if _, ok := val.(map[string]interface{})["fname"]; ok {
            name := val.(map[string]interface{})["fname"].(string)
            id := val.(map[string]interface{})["_id"].(string)
            rock.channels[id] = name
        }
    }
    return err
}

func (rock *rocketCon) requestMessageObj(mid string) map[string] interface{} {
    resp := rock.restRequest("/api/v1/chat.getMessage?msgId="+mid)
    var m map[string] interface{}
    err := json.Unmarshal(resp, &m)
    if err != nil {
        fmt.Println(err)
        return m
    }
    return m
}

func (rock *rocketCon) RequestMessage(mid string) message {
    var msg message
    obj := rock.requestMessageObj(mid)
    msg = rock.handleMessageObject(obj["message"].(map[string] interface{}))
    return msg
}

func (rock *rocketCon) SendMessage(rid string, text string) (message, error) {
    obj := map[string] interface{} {
        "method": "sendMessage",
        "params": []map[string] interface{} {
            map[string] interface{} {
                "rid": rid,
                "msg": text,
            },
        },
    }

    var msg message
    reply, err := rock.runMethod(obj)
    if err != nil {
        return msg, err
    }
    if replyObj, ok := reply["result"].(map[string]interface{}); ok {
        msg = rock.handleMessageObject(replyObj)
    }
    msg.IsMe = true
    return msg, nil
}

func (rock *rocketCon) React(mid string, emoji string) error {
    reaction := map[string] interface{} {
        "method": "setReaction",
        "params": []string {
            emoji,
            mid,
        },
    }

    _, err := rock.runMethod(reaction)
    return err
}

func (rock *rocketCon) UserDefaultStatus(status string) (error) {
    reaction := map[string] interface{} {
        "method": "UserPresence:setDefaultStatus",
        "params": []string {
            status,
        },
    }

    _, err := rock.runMethod(reaction)
    return err
}

func (rock *rocketCon) UserTemporaryStatus(status string) (error) {
    reaction := map[string] interface{} {
        "method": "UserPresence:"+status,
        "params": []string {
        },
    }

    _, err := rock.runMethod(reaction)
    return err
}

func (rock *rocketCon) ListCustomEmojis() ([]string, error) {
    emojis := make([]string,0)

    reply := rock.restRequest("/api/v1/emoji-custom.list")
    var m map[string] interface{}
    err := json.Unmarshal(reply, &m)
    if err != nil {
        return emojis, err
    }

    if _, ok := m["emojis"]; ok {
        for _, val := range m["emojis"].(map[string]interface{})["update"].([]interface{}) {
            emojis = append(emojis, fmt.Sprintf(":%s:",val.(map[string] interface{})["name"].(string)))
        }
    }
    return emojis, nil
}