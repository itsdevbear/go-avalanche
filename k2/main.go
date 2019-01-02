package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"sync"
	"syscall"
	"time"

	"github.com/gomodule/redigo/redis"
	"github.com/gorilla/websocket"
	avalanche "github.com/tyler-smith/go-avalanche"
)

var (
	networkEndpointsMu  sync.RWMutex
	networkEndpoints    []string
	debugLoggingEnabled = true
)

type jsonRPCResponse struct {
	Method string        `json:"method"`
	Params []interface{} `json:"params"`
}

func sniffTransactions(conf BCHDConfig, nodes map[avalanche.NodeID]*node) (chan struct{}, error) {
	c, _, err := websocket.DefaultDialer.Dial("ws://"+conf.Host+"/ws", nil)
	if err != nil {
		return nil, err
	}

	done := make(chan struct{})

	go func() {
		for {
			select {
			case <-done:
				c.Close()
				return
			default:
			}

			_, message, err := c.ReadMessage()
			if err != nil {
				fmt.Println("read:", err)
				return
			}

			go func() {
				resp := &jsonRPCResponse{}
				err = json.Unmarshal(message, resp)
				if err != nil {
					fmt.Println("unmarshal:", err)
					return
				}

				if resp.Method != "txaccepted" {
					return
				}

				h, ok := resp.Params[0].(string)
				if !ok {
					fmt.Println("Failed to convert to string:", resp.Params[0])
					return
				}

				for _, n := range nodes {
					n.incoming <- &tx{hash: avalanche.Hash(h), isAccepted: true}
				}

				fmt.Println("got tx:", resp.Params[0], "-", time.Now().Unix())
			}()
		}
	}()

	err = c.WriteMessage(websocket.TextMessage, []byte(`{"jsonrpc":"1.0","id":"1","method":"authenticate","params":["`+conf.User+`", "`+conf.Password+`"]}`))
	if err != nil {
		return nil, err
	}

	err = c.WriteMessage(websocket.TextMessage, []byte(`{"jsonrpc":"1.0","id":"0","method":"notifynewtransactions","params":[]}`))
	if err != nil {
		return nil, err
	}

	return done, nil
}

func maintainParticipants(rConn redis.Conn) error {
	loadNewEndpoints := func() error {
		networkEndpointsMu.Lock()
		endpoints, err := getEndpoints(rConn)
		networkEndpointsMu.Unlock()

		if err != nil {
			return err
		}

		networkEndpoints = endpoints
		return nil
	}

	go func() {
		ticker := time.NewTicker(redisEndpointTTL)
		for range ticker.C {
			fmt.Println("Refreshing participants...")
			loadNewEndpoints()
		}
	}()

	return loadNewEndpoints()
}

func main() {
	conf, err := NewConfigFromEnv()
	if err != nil {
		log.Fatal(err)
		return
	}

	debugLoggingEnabled = conf.Logging

	rConn, err := redis.Dial("tcp", conf.RedisHost)
	if err != nil {
		log.Fatal(err)
		return
	}
	defer rConn.Close()

	// Start maintaining pool of participants
	err = maintainParticipants(rConn)
	if err != nil {
		log.Fatal(err)
		return
	}

	// Create nodes
	nodes := make(map[avalanche.NodeID]*node, conf.NodeCount)
	for i := 0; i < conf.NodeCount; i++ {
		id := avalanche.NodeID(i)
		nodes[id] = newNode(id, rConn, avalanche.NewConnman())
		nodes[id].start()
	}

	// Listen for new txs to the mempool and attempt to finalize
	stopSniffing, err := sniffTransactions(conf.BCHD, nodes)
	if err != nil {
		log.Fatal(err)
		return
	}

	// Wait for exit signal
	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, syscall.SIGINT, syscall.SIGTERM)
	<-sigs

	debug("Shutting down...")

	close(stopSniffing)

	// Stop all nodes
	for _, n := range nodes {
		n.stop()
	}

	debug("Done shutting down")
}

type node struct {
	id         avalanche.NodeID
	snowball   *avalanche.Processor
	snowballMu *sync.RWMutex
	incoming   chan (*tx)
	host       string
	rConn      redis.Conn

	quitCh chan (struct{})
	doneWg *sync.WaitGroup
}

func newNode(id avalanche.NodeID, rConn redis.Conn, connman *avalanche.Connman) *node {
	return &node{
		id:         id,
		rConn:      rConn,
		snowballMu: &sync.RWMutex{},
		incoming:   make(chan (*tx), 10),
		snowball:   avalanche.NewProcessor(connman),

		quitCh: make(chan (struct{})),
		doneWg: &sync.WaitGroup{},
	}
}

func (n *node) start() {
	n.doneWg.Add(3)
	n.startProcessor()
	n.startIntake()
	err := n.startPollServer()
	if err != nil {
		n.doneWg.Done()
		panic(err)
	}
}

func (n *node) stop() {
	close(n.quitCh)
	n.doneWg.Wait()
}

func (n *node) startProcessor() {
	go func() {
		defer n.doneWg.Done()

		var (
			queries = 0
			ticker  = time.NewTicker(avalanche.AvalancheTimeStep)
		)

		for i := 0; ; i++ {
			select {
			case <-n.quitCh:
				return
			case <-ticker.C:
			}

			networkEndpointsMu.RLock()
			if len(networkEndpoints) == 0 {
				networkEndpointsMu.RUnlock()
				continue
			}
			endpoint := networkEndpoints[i%len(networkEndpoints)]
			networkEndpointsMu.RUnlock()

			// Don't query ourself
			if endpoint == n.host {
				continue
			}

			// Get invs for next query
			updates := []avalanche.StatusUpdate{}
			n.snowballMu.Lock()
			invs := n.snowball.GetInvsForNextPoll()
			n.snowballMu.Unlock()

			if len(invs) == 0 {
				continue
			}

			// Query next node
			resp, err := n.sendQuery(endpoint, invs)
			if err != nil {
				panic(err)
			}

			// Register query response
			queries++
			n.snowballMu.Lock()
			n.snowball.RegisterVotes(n.id, *resp, &updates)
			n.snowballMu.Unlock()

			// Nothing interesting happened; go to next cycle
			if len(updates) == 0 {
				continue
			}

			// Got some updates; process them
			for _, update := range updates {
				if update.Status == avalanche.StatusFinalized {
					debug("Finalized tx %s on node %d on query %d - %d", update.Hash, n.id, queries, time.Now().Unix())
				} else if update.Status == avalanche.StatusAccepted {
					debug("Accepted tx %s on node %d on query %d", update.Hash, n.id, queries)
				} else if update.Status == avalanche.StatusRejected {
					debug("Rejected tx %s on node %d on query %d", update.Hash, n.id, queries)
				} else if update.Status == avalanche.StatusInvalid {
					debug("Invalidated tx %s on node %d on query %d", update.Hash, n.id, queries)
				} else {
					fmt.Println(update.Status == avalanche.StatusAccepted)
					panic(update)
				}
			}
		}
	}()
}

// startIntake adds incoming txs to Processor
func (n *node) startIntake() {
	go func() {
		defer n.doneWg.Done()
		for {
			select {
			case <-n.quitCh:
				return
			case t := <-n.incoming:
				n.snowballMu.Lock()
				n.snowball.AddTargetToReconcile(t)
				n.snowballMu.Unlock()
			}
		}
	}()
}

func (n *node) startPollServer() error {
	mux := http.NewServeMux()
	mux.HandleFunc("/", n.respondToPoll)

	l, err := net.Listen("tcp", ":0")
	if err != nil {
		return err
	}

	n.host = "http://localhost:" + strconv.Itoa(l.Addr().(*net.TCPAddr).Port)

	err = setEndpoint(n.rConn, n.host)
	if err != nil {
		return err
	}

	go func() {
		defer n.doneWg.Done()
		fmt.Println("Node", n.id, "listening at", n.host)

		srv := http.Server{Handler: mux}
		go srv.Serve(l)
		<-n.quitCh
		fmt.Println("stopping poll server")
		srv.Shutdown(nil)
	}()

	return nil
}

func (n node) sendQuery(endpoint string, invs []avalanche.Inv) (*avalanche.Response, error) {
	body, err := json.Marshal(invs)
	if err != nil {
		return nil, err
	}

	httpResp, err := http.Post(endpoint, "text", bytes.NewBuffer(body))
	if err != nil {
		return nil, err
	}
	defer httpResp.Body.Close()

	respBytes, err := ioutil.ReadAll(httpResp.Body)
	if err != nil {
		return nil, err
	}

	resp := &avalanche.Response{}
	err = json.Unmarshal(respBytes, resp)
	if err != nil {
		return nil, err
	}

	return resp, nil
}

func (n *node) respondToPoll(w http.ResponseWriter, r *http.Request) {
	body, err := ioutil.ReadAll(r.Body)
	if err != nil {
		fmt.Printf("Error reading body: %v\n", err)
		http.Error(w, "", http.StatusBadRequest)
		return
	}

	invs := []avalanche.Inv{}
	err = json.Unmarshal(body, &invs)
	if err != nil {
		fmt.Printf("Error unmarshalling body: %v\n", err)
		http.Error(w, "", http.StatusBadRequest)
		return
	}

	votes := make([]avalanche.Vote, len(invs))

	n.snowballMu.Lock()
	defer n.snowballMu.Unlock()

	for i := 0; i < len(invs); i++ {
		n.snowball.AddTargetToReconcile(&tx{
			hash:       invs[i].TargetHash,
			isAccepted: true,
		})

		votes[i] = avalanche.NewVote(0, invs[i].TargetHash)
	}

	resp := avalanche.NewResponse(0, 0, votes)
	body, err = json.Marshal(&resp)
	if err != nil {
		fmt.Printf("Error marshalling response: %v\n", err)
		http.Error(w, "", http.StatusBadRequest)
		return
	}

	fmt.Fprintln(w, string(body))
}

// tx
type tx struct {
	hash       avalanche.Hash
	isAccepted bool
}

func (t *tx) Hash() avalanche.Hash { return t.hash }

func (t *tx) IsAccepted() bool { return t.isAccepted }

func (*tx) IsValid() bool { return true }

func (*tx) Type() string { return "tx" }

func (*tx) Score() int64 { return 1 }

func debug(str string, args ...interface{}) {
	if debugLoggingEnabled {
		fmt.Println(fmt.Sprintf(str, args...))
	}
}
