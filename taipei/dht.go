// DHT node for Taipei Torrent, for tracker-less peer information exchange.
//
// Status:
//  - able to get peers from the network
//  - uses a very simple routing table
//  - not able to _answer_ queries from remote nodes
//  - does not 'bucketize' the remote nodes
//  - does not announce torrents to the network.
//  - has only soft limits for memory growth.
//
// Usage: 
//
//  dhtNode := NewDhtNode("abcdefghij0123456789", port)  // Torrent node ID, UDP port.
//  go dhtNode.PeersRequest(infoHash)
//  -- wait --
//  infoHashPeers = <-node.PeersRequestResults
//
//  infoHashPeers will contain:
//  => map[string][]string
//  -> key = infoHash
//  -> value = slice of peer contacts in binary form. 
//
// Message types:
// - query
// - response
// - error
//
// RPCs:
//      ping:
//         see if node is reachable and save it on routing table.
//      find_node:
//	   run when DHT node count drops, or every X minutes. Just to
//   	   ensure our DHT routing table is still useful.
//      get_peers:
//	   the real deal. Iteratively queries DHT nodes and find new
//         sources for a particular infohash.
//	announce_peer:
//         announce that this node is downloading a torrent.
//
// Reference:
//     http://www.bittorrent.org/beps/bep_0005.html
//

package taipei

import (
	"errors"
	"expvar"
	"flag"
	"fmt"
	"log"
	"math/rand"
	"net"
	"sort"
	"time"
)

const (
	// How many nodes to contact initially each time we are asked to find new torrent peers.
	NUM_INCREMENTAL_NODE_QUERIES = 5
	// If we have less than so known nodes for a particular peer, be
	// aggressive about collecting new ones. Otherwise, wait for the
	// torrent client to ask us. (currently does not consider reachability).
	MIN_INFOHASH_PEERS = 100
	// Consider a node stale if it has more than this number of oustanding queries from us.
	MAX_NODE_PENDING_QUERIES = 5
	// Ask the same infoHash to a node after a long time.
	MIN_SECONDS_NODE_REPEAT_QUERY = 30 * time.Minute
)

var dhtRouter string

func init() {
	flag.StringVar(&dhtRouter, "dhtRouter", "67.215.242.138:6881",
		"IP:Port address of the DHT router used to bootstrap the DHT network.")
}

// DhtEngine should be created by NewDhtNode(). It provides DHT features to a torrent client, such as finding new peers
// for torrent downloads without requiring a tracker. The client can only use the public (first letter uppercase)
// channels for communicating with the DHT goroutines.
type DhtEngine struct {
	peerID        string
	port          int
	remoteNodes   map[string]*DhtRemoteNode // key == address 
	infoHashPeers map[string]map[string]int // key1 == infoHash, key2 == address in binary form. value=ignored.

	// Public channels:
	remoteNodeAcquaintance chan *DhtNodeCandidate
	peersRequest           chan string
	PeersRequestResults    chan map[string][]string // key = infohash, v = slice of peers.
}

func NewDhtNode(nodeId string, port int) (node *DhtEngine, err error) {
	node = &DhtEngine{
		peerID:                 nodeId,
		port:                   port,
		remoteNodes:            make(map[string]*DhtRemoteNode),
		PeersRequestResults:    make(chan map[string][]string, 1),
		remoteNodeAcquaintance: make(chan *DhtNodeCandidate),
		peersRequest:           make(chan string, 1), // buffer to avoid deadlock.
		infoHashPeers:          make(map[string]map[string]int),
	}
	return
}

type DhtNodeCandidate struct {
	id      string
	address string
}

func (d *DhtEngine) newRemoteNode(id string, hostPort string) (r *DhtRemoteNode) {
	address, err := net.ResolveUDPAddr("udp", hostPort)
	if err != nil {
		return nil
	}
	r = &DhtRemoteNode{
		address:        address,
		lastQueryID:    rand.Intn(255) + 1, // Doesn't have to be crypto safe.
		id:             id,
		localNode:      d,
		reachable:      false,
		pendingQueries: map[string]*queryType{},
		pastQueries:    map[string]*queryType{},
	}
	nodesVar.Add(hostPort, 1)
	return

}

// getRemodeNode returns the DhtRemoteNode with the provided address. Creates a new object if necessary.
func (d *DhtEngine) getOrCreateRemoteNode(address string) (r *DhtRemoteNode) {
	var ok bool
	if r, ok = d.remoteNodes[address]; !ok {
		r = d.newRemoteNode("", address)
		d.remoteNodes[address] = r
	}
	return r
}

func (d *DhtEngine) ping(address string, async bool) (err error) {
	// TODO: should translate to an IP first.
	r := d.getOrCreateRemoteNode(address)
	log.Printf("DHT: ping => %+v\n", r)
	t := r.newQuery("ping")
	p, _ := r.encodedPing(t)
	if async {
		go r.sendMsg(p)
	} else {
		_, err = r.sendMsg(p)
		if err != nil {
			log.Println("DHT: Handshake error with node", r.address, err.Error())
		}
	}
	return
}

// PeersRequest tells the DHT to search for more peers for the infoHash
// provided. Must be called as a goroutine.
func (d *DhtEngine) PeersRequest(ih string) {
	// Signals the main DHT goroutine.
	d.peersRequest <- ih
}

func (d *DhtEngine) RemoteNodeAcquaintance(n *DhtNodeCandidate) {
	d.remoteNodeAcquaintance <- n
}

// DoDht is the DHT node main loop and should be run as a goroutine by the torrent client.
func (d *DhtEngine) DoDht() {
	socketChan := make(chan packetType)
	socket, err := listen(d.port)
	if err != nil {
		return
	}
	go readFromSocket(socket, socketChan)

	d.bootStrapNetwork()

	log.Println("DHT: Starting DHT node.")
	for {
		select {
		case helloNode := <-d.remoteNodeAcquaintance:
			// We've got a new node id. We need to:
			// - see if we know it already, skip accordingly.
			// - ping it and see if it's reachable. Ignore otherwise.
			// - save it on our list of good nodes.
			// - later, we'll implement bucketing, etc.
			if _, ok := d.remoteNodes[helloNode.id]; !ok {
				_ = d.newRemoteNode(helloNode.id, helloNode.address)
				d.ping(helloNode.address, true)
			}

		case needPeers := <-d.peersRequest:
			// torrent server is asking for more peers for a particular infoHash.  Ask the closest nodes for
			// directions. The goroutine will write into the PeersNeededResults channel.
			log.Println("DHT: torrent client asking for more peers. Calling GetPeers().")
			d.GetPeers(needPeers)
			// XXX stats
			c := 0
			for _, node := range d.remoteNodes {
				if node.reachable {
					c++
				}
			}
			log.Println("DHT: Reachable hosts", c)
		case p := <-socketChan:
			addr := p.raddr.String()
			// XXX needs to work for dialogs we didnt initiate.
			r, _ := readResponse(p)
			node, ok := d.remoteNodes[addr]
			if !ok {
				log.Println("DHT: Contacted by a host we don't know:", addr)
				log.Println("DHT: -> ignoring this for now because I'm dumb.")
				continue
			}
			// Fix the node ID. Or is there a better time do it?
			if node.id == "" {
				node.id = r.R.Id
			}
			switch {
			// Response.
			case r.Y == "r":
				if query, ok := node.pendingQueries[r.T]; ok {
					node.reachable = true
					node.lastTime = time.Now()
					if _, ok := d.infoHashPeers[query.ih]; !ok {
						d.infoHashPeers[query.ih] = map[string]int{}
					}
					switch {
					case query.Type == "ping":
						// served its purpose, nothing else to be done.
					case query.Type == "get_peers":
						d.processGetPeerResults(node, r)
					default:
						log.Println("DHT: Unknown query type:", query.Type)
					}
					node.pastQueries[r.T] = query
					delete(node.pendingQueries, r.T)
				} else {
					// XXX
					log.Println("DHT: Unknown query id:", r.T)
				}
			default:
				log.Println("DHT: Unknown DHT query. Forgive me for being a bit illiterate.")
			}
		}
	}
}

// Process another node's response to a get_peers query. If the response contains peers, send them to the Torrent
// engine, our client, using the DhtEngine.PeersRequestResults channel. If it contains closest nodes, query them if we
// still need it.
func (d *DhtEngine) processGetPeerResults(node *DhtRemoteNode, resp responseType) {
	query, _ := node.pendingQueries[resp.T]
	if resp.R.Values != nil {
		peers := make([]string, 0)
		for _, peerContact := range resp.R.Values {
			if _, ok := d.infoHashPeers[query.ih][peerContact]; !ok {
				// Finally, a new peer.
				d.infoHashPeers[query.ih][peerContact] = 0
				peers = append(peers, peerContact)
			}
		}
		if len(peers) > 0 {
			result := map[string][]string{query.ih: peers}
			totalPeers.Add(int64(len(peers)))
			log.Println("DHT: totalPeers:", totalPeers.String())
			d.PeersRequestResults <- result
		}
	}
	if resp.R.Nodes != "" {
		for id, address := range parseNodesString(resp.R.Nodes) {
			// XXX
			log.Printf("DHT: Got node reference: %x@%v from %x%v.", id, address, node.id, node.address)
			// If it's in our routing table already, ignore it.
			if _, ok := d.remoteNodes[address]; ok {
				totalDupes.Add(1)
				// XXX Gotta improve things so we stop receiving so many dupes. Waste.
				log.Println("DHT: total dupes:", totalDupes.String())
			} else {
				log.Println("DHT: and it is actually new. Interesting. LEN:", len(d.infoHashPeers[query.ih]))
				nr := d.newRemoteNode(id, address)
				d.remoteNodes[address] = nr
				if len(d.infoHashPeers[query.ih]) < MIN_INFOHASH_PEERS {
					d.GetPeers(query.ih)
				} else {
					log.Println("DHT: .. just saving in the routing table")
				}
			}
		}
	}
}

// Calculates the distance between two hashes. In DHT/Kademlia, "distance" is the XOR of the torrent infohash and the
// peer node ID.
func hashDistance(id1 string, id2 string) (distance string, err error) {
	d := make([]byte, 20)
	if id1 == id2 {
		err = errors.New("===> Zero distance between identical IDs")
	}
	if len(id1) != 20 || len(id2) != 20 {
		err = errors.New(fmt.Sprintf("idDistance unexpected id length(s): %d %d", len(id1), len(id2)))
	} else {
		for i := 0; i < 20; i++ {
			d[i] = id1[i] ^ id2[i]
		}
		distance = string(d)
	}
	return
}

// Implements sort.Interface to find the closest nodes for a particular
// infoHash.
type nodeDistances struct {
	infoHash string
	nodes    []*DhtRemoteNode
}

func (n *nodeDistances) Len() int {
	return len(n.nodes)
}
func (n *nodeDistances) Less(i, j int) bool {
	// XXX: what to do if an error occurred?
	ii, _ := hashDistance(n.infoHash, n.nodes[i].id)
	jj, _ := hashDistance(n.infoHash, n.nodes[j].id)
	return ii < jj
}
func (n *nodeDistances) Swap(i, j int) {
	ni := n.nodes[i]
	nj := n.nodes[j]
	n.nodes[i] = nj
	n.nodes[j] = ni
}

// Asks for more peers for a torrent. Runs on the main dht goroutine so it must
// finish quickly. Currently this does not implement the official DHT routing
// table from the spec, but my own thing :-P.
//
// The basic principle is to store as many node addresses as possible, even if their hash is distant from other nodes we asked.
func (d *DhtEngine) GetPeers(infoHash string) {
	ih := infoHash
	if d.remoteNodes == nil {
		log.Println("DHT: Error: no remote nodes are known yet.")
		return
	}
	targets := &nodeDistances{infoHash, make([]*DhtRemoteNode, 0, len(d.remoteNodes))}
	for _, r := range d.remoteNodes {
		// Skip nodes with pending queries. First, we don't want to flood them, but most importantly they are
		// probably unreachable. We just need to make sure we clean the pendingQueries map when appropriate.
		if len(r.pendingQueries) > MAX_NODE_PENDING_QUERIES {
			log.Println("DHT: Skipping because there are too many queries pending for this dude.")
			log.Println("DHT: This shouldn't happen because we should have stopped trying already. Might be a BUG.")
			for _, q := range r.pendingQueries {
				log.Printf("DHT: %v=>%x\n", q.Type, q.ih)
			}
			continue
		}
		// Skip if we are already asking them for this infoHash.
		skip := false
		for _, q := range r.pendingQueries {
			if q.Type == "get_peers" && q.ih == infoHash {
				skip = true
			}
		}
		// Skip if we asked for this infoHash recently.
		for _, q := range r.pastQueries {
			if q.Type == "get_peers" && q.ih == infoHash {
				ago := time.Now().Sub(r.lastTime)
				if ago < MIN_SECONDS_NODE_REPEAT_QUERY {
					skip = true
				} else {
					// This is an act of desperation. Query
					// them again.  Most likely this will
					// only generate dupes, but it's worth
					// a try.
					log.Printf("Re-sending get_peers. Last time: %v (%v ago) %v",
						r.lastTime.String(), ago.Seconds(), ago > 10*time.Second)
				}
			}
		}
		if !skip {
			targets.nodes = append(targets.nodes, r)
		}
	}
	log.Printf("DHT: Candidate nodes for asking: %d", len(targets.nodes))
	log.Printf("DHT: Currently know %d nodes", len(d.remoteNodes))
	// Go rules!
	sort.Sort(targets)
	for i := 0; i < NUM_INCREMENTAL_NODE_QUERIES && i < len(targets.nodes); i++ {
		r := targets.nodes[i]
		t := r.newQuery("get_peers")
		r.pendingQueries[t].ih = ih
		m, _ := r.encodedGetPeers(t, ih)
		totalGetPeers.Add(1)
		go r.sendMsg(m)
	}
	log.Println("DHT: totalGetPeers", totalGetPeers.String())
}

// Debugging information:
// Which nodes we contacted.
var nodesVar = expvar.NewMap("totalNodes")
var totalDupes = expvar.NewInt("totalDupes")
var totalPeers = expvar.NewInt("totalPeers")
var totalGetPeers = expvar.NewInt("totalGetPeers")

func (d *DhtEngine) bootStrapNetwork() error {
	return d.ping(dhtRouter, false)
}

// TODO: Create a proper routing table with buckets, per the protocol.
// TODO: Save routing table on disk to be preserved between instances.
// TODO: Cleanup bad nodes from time to time.

// === Notes ==
//
// Everything is running in a single goroutine so synchronization is not an issue. There are exceptions of methods
// that may run in their own goroutine. 
// - sendMsg()
// - readFromSocket()
//
// There is a risk of deadlock between the client asking for peers and the server trying to send a response. I added a
// buffer but I'm not sure if that's enough.
// UPDATE: it's not. http://pastebin.com/Yy5G1VGJ

func init() {
	//	DhtStats.engines = make([]*DhtEngine, 1, 10)
	//	expvar.Publish("dhtengine", expvar.StringFunc(dhtstats))
	//	expvar.NewMap("nodes")
}
