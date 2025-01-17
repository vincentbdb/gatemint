// Copyright (C) 2019 Algorand, Inc.
// This file is part of go-algorand
//
// go-algorand is free software: you can redistribute it and/or modify
// it under the terms of the GNU Affero General Public License as
// published by the Free Software Foundation, either version 3 of the
// License, or (at your option) any later version.
//
// go-algorand is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
// GNU Affero General Public License for more details.
//
// You should have received a copy of the GNU Affero General Public License
// along with go-algorand.  If not, see <https://www.gnu.org/licenses/>.

package agreement

import (
	"bytes"
	"crypto/sha256"
	"fmt"
	"math/rand"
	"os"
	"reflect"
	"runtime"
	"strconv"
	"testing"
	"time"

	"github.com/algorand/go-deadlock"
	"github.com/stretchr/testify/require"

	"github.com/vincentbdb/go-algorand/config"
	"github.com/vincentbdb/go-algorand/crypto"
	"github.com/vincentbdb/go-algorand/data/account"
	"github.com/vincentbdb/go-algorand/data/basics"
	"github.com/vincentbdb/go-algorand/logging"
	"github.com/vincentbdb/go-algorand/protocol"
	"github.com/vincentbdb/go-algorand/util/db"
	"github.com/vincentbdb/go-algorand/util/timers"
)

type testingClock struct {
	mu deadlock.Mutex

	zeroes uint

	TA map[time.Duration]chan time.Time // TimeoutAt

	monitor *coserviceMonitor
}

func makeTestingClock(m *coserviceMonitor) *testingClock {
	c := new(testingClock)
	c.TA = make(map[time.Duration]chan time.Time)
	c.monitor = m
	return c
}

func (c *testingClock) Zero() timers.Clock {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.zeroes++
	c.TA = make(map[time.Duration]chan time.Time)
	c.monitor.clearClock()
	return c
}

func (c *testingClock) TimeoutAt(d time.Duration) <-chan time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()

	ta := c.TA[d]
	if ta == nil {
		c.TA[d] = make(chan time.Time)
		ta = c.TA[d]
	}
	return ta
}

func (c *testingClock) Encode() []byte {
	return nil
}

func (c *testingClock) Decode([]byte) (timers.Clock, error) {
	return makeTestingClock(nil), nil // TODO
}

func (c *testingClock) prepareToFire() {
	c.monitor.inc(clockCoserviceType)
}

func (c *testingClock) fire(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.TA[d] == nil {
		c.TA[d] = make(chan time.Time)
	}
	close(c.TA[d])
}

type simpleKeyManager []account.Participation

func (m simpleKeyManager) Keys() []account.Participation {
	var km []account.Participation
	for _, acc := range m {
		km = append(km, acc)
	}
	return km
}

// HasLiveKeys returns true if we have any Participation
// keys valid for the specified round range (inclusive)
func (m simpleKeyManager) HasLiveKeys(from, to basics.Round) bool {
	for _, acc := range m {
		if acc.OverlapsInterval(from, to) {
			return true
		}
	}
	return false
}

func (m simpleKeyManager) DeleteOldKeys(basics.Round) {
	// noop
}

type testingNetwork struct {
	validator BlockValidator

	voteMessages    []chan Message
	payloadMessages []chan Message
	bundleMessages  []chan Message

	mu deadlock.Mutex // guards connected, nextHandle, source, and monitors

	connected  [][]bool // symmetric
	nextHandle int
	source     map[MessageHandle]nodeID
	monitors   map[nodeID]*coserviceMonitor

	// used for extra tests
	dropSoftVotes     bool
	dropSlowNextVotes bool
	dropVotes         bool
	certVotePocket    chan<- multicastParams
	softVotePocket    chan<- multicastParams
	compoundPocket    chan<- multicastParams
	partitionedNodes  map[nodeID]bool
	crownedNodes      map[nodeID]bool
	interceptFn       multicastInterceptFn
}

type testingNetworkEndpoint struct {
	parent *testingNetwork
	id     nodeID

	voteMessages    chan Message
	payloadMessages chan Message
	bundleMessages  chan Message

	monitor *coserviceMonitor
}

type nodeID int

// bufferCapacity is per channel
func makeTestingNetwork(nodes int, bufferCapacity int, validator BlockValidator) *testingNetwork {
	n := new(testingNetwork)

	n.validator = validator

	n.voteMessages = make([]chan Message, nodes)
	n.payloadMessages = make([]chan Message, nodes)
	n.bundleMessages = make([]chan Message, nodes)
	n.source = make(map[MessageHandle]nodeID)
	n.monitors = make(map[nodeID]*coserviceMonitor)

	for i := 0; i < nodes; i++ {
		n.voteMessages[i] = make(chan Message, bufferCapacity)
		n.payloadMessages[i] = make(chan Message, bufferCapacity)
		n.bundleMessages[i] = make(chan Message, bufferCapacity)

		m := new(coserviceMonitor)
		m.id = i
		n.monitors[nodeID(i)] = m
	}

	n.connected = make([][]bool, nodes)
	for i := 0; i < nodes; i++ {
		n.connected[i] = make([]bool, nodes)
		for j := 0; j < nodes; j++ {
			n.connected[i][j] = true
		}
	}

	return n
}

type multicastInterceptFn func(params multicastParams) multicastParams
type multicastParams struct {
	tag     protocol.Tag
	data    []byte
	source  nodeID
	exclude nodeID
}

func (n *testingNetwork) multicast(tag protocol.Tag, data []byte, source nodeID, exclude nodeID) {
	// fmt.Println("mc", source, "x", exclude)
	n.mu.Lock()
	defer n.mu.Unlock()

	if n.interceptFn != nil {
		out := n.interceptFn(multicastParams{tag, data, source, exclude})
		tag, data, source, exclude = out.tag, out.data, out.source, out.exclude
	}

	if n.dropSoftVotes || n.dropSlowNextVotes || n.dropVotes || n.certVotePocket != nil || n.softVotePocket != nil || n.compoundPocket != nil || n.crownedNodes != nil {
		if tag == protocol.ProposalPayloadTag {
			r := bytes.NewBuffer(data)

			var tp transmittedPayload
			err := protocol.DecodeStream(r, &tp)
			if err != nil {
				panic(err)
			}

			if n.compoundPocket != nil {
				n.compoundPocket <- multicastParams{tag, data, source, exclude}
				return
			}
		}

		if tag == protocol.AgreementVoteTag {
			r := bytes.NewBuffer(data)

			var uv unauthenticatedVote
			err := protocol.DecodeStream(r, &uv)
			if err != nil {
				panic(err)
			}

			if n.certVotePocket != nil && uv.R.Step == cert {
				n.certVotePocket <- multicastParams{tag, data, source, exclude}
				return
			}

			if n.softVotePocket != nil && uv.R.Step == soft {
				n.softVotePocket <- multicastParams{tag, data, source, exclude}
				return
			}

			if n.dropVotes {
				return
			}

			if n.dropSoftVotes && uv.R.Step == soft {
				return
			}

			if n.dropSlowNextVotes && uv.R.Step >= next && uv.R.Step != late && uv.R.Step != redo && uv.R.Step != down {
				return
			}
		}
	}

	n.nextHandle++
	handle := new(int)
	*handle = n.nextHandle
	n.source[handle] = source

	var msgChans []chan Message
	switch tag {
	case protocol.AgreementVoteTag:
		msgChans = n.voteMessages
	case protocol.VoteBundleTag:
		msgChans = n.bundleMessages
	case protocol.ProposalPayloadTag:
		msgChans = n.payloadMessages
	case protocol.UnknownMsgTag:
		// We use this intentionally - just drop it
		return
	default:
		panic("bad broadcast call")
	}

	for i, connected := range n.connected[source] {
		peerid := nodeID(i)
		if peerid == source {
			continue
		}
		if peerid == exclude {
			continue
		}
		if !connected {
			continue
		}
		if n.partitionedNodes != nil {
			if n.partitionedNodes[source] != n.partitionedNodes[nodeID(i)] {
				continue
			}
		}
		if n.crownedNodes != nil {
			if !n.crownedNodes[nodeID(i)] {
				return
			}
		}

		// we should have incremented tokenizerCoserviceType
		n.monitors[peerid].inc(tokenizerCoserviceType)
		select {
		case msgChans[peerid] <- Message{MessageHandle: handle, Data: data}:
			// fmt.Println("transmit-success", source, "->", peerid)
		default:
			logging.Base().Warn("message dropped during test")
			n.monitors[peerid].dec(tokenizerCoserviceType)
			// fmt.Println("transmit-failure", source, "->", peerid)
		}
	}
}

func (n *testingNetwork) dropAllSoftVotes() {
	n.mu.Lock()
	defer n.mu.Unlock()

	n.dropSoftVotes = true
}

func (n *testingNetwork) dropAllSlowNextVotes() {
	n.mu.Lock()
	defer n.mu.Unlock()

	n.dropSlowNextVotes = true
}

func (n *testingNetwork) dropAllVotes() {
	n.mu.Lock()
	defer n.mu.Unlock()

	n.dropVotes = true
}

func (n *testingNetwork) pocketAllCertVotes(ch chan<- multicastParams) (closeFn func()) {
	n.certVotePocket = ch
	return func() {
		close(ch)
	}
}

func (n *testingNetwork) pocketAllSoftVotes(ch chan<- multicastParams) (closeFn func()) {
	n.softVotePocket = ch
	return func() {
		close(ch)
	}
}

func (n *testingNetwork) pocketAllCompound(ch chan<- multicastParams) (closeFn func()) {
	n.compoundPocket = ch
	return func() {
		close(ch)
	}
}

func (n *testingNetwork) repairAll() {
	n.mu.Lock()
	defer n.mu.Unlock()

	n.dropSoftVotes = false
	n.dropSlowNextVotes = false
	n.dropVotes = false
	n.certVotePocket = nil
	n.softVotePocket = nil
	n.compoundPocket = nil
	n.partitionedNodes = nil
	n.crownedNodes = nil
	n.interceptFn = nil
}

func (n *testingNetwork) disconnect(a nodeID, b nodeID) {
	n.mu.Lock()
	defer n.mu.Unlock()

	n.connected[a][b] = false
	n.connected[b][a] = false
}

// Set the given list of nodes as a partition; heal whatever previous
// partition existed.
func (n *testingNetwork) partition(part ...nodeID) {
	n.mu.Lock()
	defer n.mu.Unlock()
	// different mechanism than n.connected map
	n.partitionedNodes = make(map[nodeID]bool)
	for i := 0; i < len(part); i++ {
		n.partitionedNodes[part[i]] = true
	}
}

// Only deliver messages to the given set of nodes
func (n *testingNetwork) crown(prophets ...nodeID) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.crownedNodes = make(map[nodeID]bool)
	for i := 0; i < len(prophets); i++ {
		n.crownedNodes[nodeID(i)] = true
	}
}

// intercept messages from the given sources, replacing them with our own.
// if, in the returned params, the message is tagged UnknownMsgTag, the testing
// network drops the message.
func (n *testingNetwork) intercept(f multicastInterceptFn) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.interceptFn = f
}

func (n *testingNetwork) sourceOf(h MessageHandle) nodeID {
	n.mu.Lock()
	defer n.mu.Unlock()
	if _, noint := h.(*int); !noint {
		panic(fmt.Errorf("h isn't a *int; %v", reflect.TypeOf(h)))
	}
	return n.source[h]
}

func (n *testingNetwork) testingNetworkEndpoint(id nodeID) *testingNetworkEndpoint {
	e := new(testingNetworkEndpoint)
	e.id = id
	e.parent = n
	e.voteMessages = n.voteMessages[id]
	e.payloadMessages = n.payloadMessages[id]
	e.bundleMessages = n.bundleMessages[id]
	e.monitor = n.monitors[id]
	return e
}

// this allows us to put the activity into a busy state until the message on the queue is actually processed
func (n *testingNetwork) prepareAllMulticast() {
	n.mu.Lock()
	defer n.mu.Unlock()
	for i := 0; i < len(n.monitors); i++ {
		n.monitors[nodeID(i)].inc(networkCoserviceType)
	}
}

func (n *testingNetwork) finishAllMulticast() {
	n.mu.Lock()
	defer n.mu.Unlock()
	for i := 0; i < len(n.monitors); i++ {
		n.monitors[nodeID(i)].dec(networkCoserviceType)
	}
}

func (e *testingNetworkEndpoint) Messages(tag protocol.Tag) <-chan Message {
	switch tag {
	case protocol.AgreementVoteTag:
		return e.voteMessages
	case protocol.VoteBundleTag:
		return e.bundleMessages
	case protocol.ProposalPayloadTag:
		return e.payloadMessages
	default:
		panic("bad messages call")
	}
}

func (e *testingNetworkEndpoint) Broadcast(tag protocol.Tag, data []byte) error {
	e.parent.multicast(tag, data, e.id, e.id)
	return nil
}

func (e *testingNetworkEndpoint) Relay(h MessageHandle, t protocol.Tag, data []byte) error {
	sourceID := e.id
	if _, isMsg := h.(*int); isMsg {
		sourceID = e.parent.sourceOf(h)
	}

	e.parent.multicast(t, data, e.id, sourceID)
	return nil
}

func (e *testingNetworkEndpoint) Disconnect(h MessageHandle) {
	if _, isMsg := h.(*int); !isMsg {
		return
	}

	sourceID := e.parent.sourceOf(h)
	e.parent.disconnect(e.id, sourceID)
}

type activityMonitor struct {
	deadlock.Mutex

	busy bool

	sums      map[nodeID]uint
	listeners map[nodeID]coserviceListener

	activity chan struct{}
	quiet    chan struct{}
}

func makeActivityMonitor() (m *activityMonitor) {
	m = new(activityMonitor)
	m.sums = make(map[nodeID]uint)
	m.listeners = make(map[nodeID]coserviceListener)
	m.activity = make(chan struct{}, 1000)
	m.quiet = make(chan struct{}, 1000)
	return
}

func (m *activityMonitor) coserviceListener(id nodeID) coserviceListener {
	m.Lock()
	defer m.Unlock()

	if m.listeners[id] == nil {
		m.listeners[id] = amCoserviceListener{id: id, activityMonitor: m}
	}
	return m.listeners[id]
}

func (m *activityMonitor) sum() (s uint) {
	for _, a := range m.sums {
		s += a
	}
	return
}

func (m *activityMonitor) dump() {
	m.Lock()
	defer m.Unlock()

	fmt.Println("activityMonitor: dump")
	for n, s := range m.sums {
		fmt.Printf("%v: %v\n", n, s)
	}
}

func (m *activityMonitor) waitForActivity() {
	<-m.activity
}

func (m *activityMonitor) waitForQuiet() {
	select {
	case <-m.quiet:
	case <-time.After(5 * time.Second):
		m.dump()

		var buf [1000000]byte
		n := runtime.Stack(buf[:], true)
		fmt.Println("Printing goroutine dump of size", n)
		fmt.Println(string(buf[:n]))

		panic("timed out waiting for quiet...")
	}
}

type amCoserviceListener struct {
	id nodeID
	*activityMonitor
}

func (l amCoserviceListener) inc(sum uint) {
	l.Lock()
	defer l.Unlock()

	l.activityMonitor.sums[l.id] = sum

	if !l.busy {
		l.activity <- struct{}{}
		l.busy = true
	}
}

func (l amCoserviceListener) dec(sum uint) {
	l.Lock()
	defer l.Unlock()

	l.activityMonitor.sums[l.id] = sum

	if l.busy && l.sum() == 0 {
		l.quiet <- struct{}{}
		l.busy = false
	}
}

// copied from fuzzer/ledger_test.go. We can merge once a refactor seems necessary.
func generatePseudoRandomVRF(keynum int) *crypto.VRFSecrets {
	seed := [32]byte{}
	seed[0] = byte(keynum % 255)
	seed[1] = byte(keynum / 255)
	pk, sk := crypto.VrfKeygenFromSeed(seed)
	return &crypto.VRFSecrets{
		PK: pk,
		SK: sk,
	}
}

func createTestAccountsAndBalances(t *testing.T, numNodes int, rootSeed []byte) (accounts []account.Participation, balances map[basics.Address]basics.BalanceRecord) {
	off := int(rand.Uint32() >> 2) // prevent name collision from running tests more than once

	// system state setup: keygen, stake initialization
	accounts = make([]account.Participation, numNodes)
	balances = make(map[basics.Address]basics.BalanceRecord, numNodes)
	var seed crypto.Seed
	copy(seed[:], rootSeed)

	for i := 0; i < numNodes; i++ {
		var rootAddress basics.Address
		// add new account rootAddress to db
		{
			rootAccess, err := db.MakeAccessor(t.Name()+"root"+strconv.Itoa(i+off), false, true)
			if err != nil {
				panic(err)
			}
			seed = sha256.Sum256(seed[:]) // rehash every node to get different root addresses
			root, err := account.ImportRoot(rootAccess, seed)
			if err != nil {
				panic(err)
			}
			rootAddress = root.Address()
		}

		var v *crypto.OneTimeSignatureSecrets
		firstValid := basics.Round(0)
		lastValid := basics.Round(1000)
		// generate new participation keys
		{
			// Compute how many distinct participation keys we should generate
			keyDilution := config.Consensus[protocol.ConsensusCurrentVersion].DefaultKeyDilution
			firstID := basics.OneTimeIDForRound(firstValid, keyDilution)
			lastID := basics.OneTimeIDForRound(lastValid, keyDilution)
			numBatches := lastID.Batch - firstID.Batch + 1

			// Generate them
			v = crypto.GenerateOneTimeSignatureSecrets(firstID.Batch, numBatches)
		}

		// save partkeys to db
		{
			partAccess, err := db.MakeAccessor(t.Name()+"part"+strconv.Itoa(i+off), false, true)
			if err != nil {
				panic(err)
			}
			accounts[i] = account.Participation{
				Parent:     rootAddress,
				VRF:        generatePseudoRandomVRF(i),
				Voting:     v,
				FirstValid: firstValid,
				LastValid:  lastValid,
				Store:      partAccess,
			}
			err = accounts[i].Persist()
			if err != nil {
				panic(err)
			}
		}

		// expose balances for future ledger creation
		acctData := basics.AccountData{
			Status:      basics.Online,
			MicroAlgos:  basics.MicroAlgos{Raw: 1000000},
			VoteID:      accounts[i].VotingSecrets().OneTimeSignatureVerifier,
			SelectionID: accounts[i].VRFSecrets().PK,
		}
		balances[rootAddress] = basics.BalanceRecord{
			Addr:        rootAddress,
			AccountData: acctData,
		}
	}
	return
}

const (
	firstFPR  = 436854775807
	secondFPR = 736854775807
)

// testingRand always returns max uint64 / 2.
type testingRand struct{}

func (testingRand) Uint64() uint64 {
	var zero uint64
	maxuint64 := zero - 1
	return maxuint64 / 2
}

func setupAgreement(t *testing.T, numNodes int, traceLevel traceLevel, ledgerFactory func(map[basics.Address]basics.BalanceRecord) Ledger) (*testingNetwork, Ledger, func(), []*Service, []timers.Clock, []Ledger, *activityMonitor) {
	bufCap := 1000 // max number of buffered messages

	// system state setup: keygen, stake initialization
	accounts, balances := createTestAccountsAndBalances(t, numNodes, (&[32]byte{})[:])
	baseLedger := makeTestLedger(balances)

	// logging
	log := logging.Base()
	f, _ := os.Create(t.Name() + ".log")
	log.SetJSONFormatter()
	log.SetOutput(f)
	log.SetLevel(logging.Debug)

	// node setup
	clocks := make([]timers.Clock, numNodes)
	ledgers := make([]Ledger, numNodes)
	dbAccessors := make([]db.Accessor, numNodes)
	services := make([]*Service, numNodes)
	var validator testBlockValidator
	baseNetwork := makeTestingNetwork(numNodes, bufCap, validator)
	am := makeActivityMonitor()

	for i := 0; i < numNodes; i++ {
		accessor, err := db.MakeAccessor(t.Name()+"_"+strconv.Itoa(i)+"_crash.db", false, true)
		if err != nil {
			panic(err)
		}
		dbAccessors[i] = accessor

		m := baseNetwork.monitors[nodeID(i)]
		m.coserviceListener = am.coserviceListener(nodeID(i))
		clocks[i] = makeTestingClock(m)
		ledgers[i] = ledgerFactory(balances)
		keys := simpleKeyManager(accounts[i : i+1])
		endpoint := baseNetwork.testingNetworkEndpoint(nodeID(i))
		ilog := log.WithFields(logging.Fields{"Source": "service-" + strconv.Itoa(i)})

		params := Parameters{
			Logger:         ilog,
			Ledger:         ledgers[i],
			Network:        endpoint,
			KeyManager:     keys,
			BlockValidator: validator,
			BlockFactory:   testBlockFactory{Owner: i},
			Clock:          clocks[i],
			Accessor:       accessor,
			Local:          config.Local{CadaverSizeTarget: 10000000},
			RandomSource:   &testingRand{},
		}

		cadaverFilename := fmt.Sprintf("%v-%v", t.Name(), i)
		os.Remove(cadaverFilename + ".cdv")
		os.Remove(cadaverFilename + ".cdv.archive")

		services[i] = MakeService(params)
		services[i].tracer.cadaver.baseFilename = cadaverFilename
		services[i].tracer.level = traceLevel
		services[i].tracer.tag = strconv.Itoa(i)

		services[i].monitor = m
		services[i].demux.monitor = m
		pn := services[i].loopback.(asyncPseudonode)
		pn.monitor = m
		services[i].loopback = pn
		m.inc(demuxCoserviceType)
	}

	cleanupFn := func() {
		for _, accessor := range dbAccessors {
			defer accessor.Close()
		}

		if r := recover(); r != nil {
			for n, c := range clocks {
				fmt.Printf("node-%v:\n", n)
				c.(*testingClock).monitor.dump()
			}
			panic(r)
		}
	}
	return baseNetwork, baseLedger, cleanupFn, services, clocks, ledgers, am
}

func (m *coserviceMonitor) dump() {
	m.Mutex.Lock()
	defer m.Mutex.Unlock()

	for t, n := range m.c {
		fmt.Printf(" %v: %v\n", t, n)
	}
	return
}

func (m *coserviceMonitor) clearClock() {
	if m == nil {
		return
	}

	m.Mutex.Lock()
	defer m.Mutex.Unlock()

	if m.c == nil {
		m.c = make(map[coserviceType]uint)
	}
	m.c[clockCoserviceType] = 0

	if m.coserviceListener != nil {
		m.coserviceListener.dec(m.sum())
	}
}

func expectNewPeriod(clocks []timers.Clock, zeroes uint) (newzeroes uint) {
	zeroes++
	for i := range clocks {
		if clocks[i].(*testingClock).zeroes != zeroes {
			errstr := fmt.Sprintf("unexpected number of zeroes: %v != %v", clocks[i].(*testingClock).zeroes, zeroes)
			panic(errstr)
		}
	}
	return zeroes
}

func expectNoNewPeriod(clocks []timers.Clock, zeroes uint) (newzeroes uint) {
	for i := range clocks {
		if clocks[i].(*testingClock).zeroes != zeroes {
			errstr := fmt.Sprintf("unexpected number of zeroes: %v != %v", clocks[i].(*testingClock).zeroes, zeroes)
			panic(errstr)
		}
	}
	return zeroes
}

func triggerGlobalTimeout(d time.Duration, clocks []timers.Clock, activityMonitor *activityMonitor) {
	for i := range clocks {
		clocks[i].(*testingClock).prepareToFire()
	}
	for i := range clocks {
		clocks[i].(*testingClock).fire(d)
	}
	activityMonitor.waitForActivity()
	activityMonitor.waitForQuiet()
}

func runRound(clocks []timers.Clock, activityMonitor *activityMonitor, zeroes uint) (newzeroes uint) {
	triggerGlobalTimeout(filterTimeout, clocks, activityMonitor)
	return expectNewPeriod(clocks, zeroes)
}

func sanityCheck(startRound round, numRounds round, ledgers []Ledger) {
	for i := range ledgers {
		if ledgers[i].NextRound() != startRound+numRounds {
			panic("did not progress numRounds rounds")
		}
	}

	for j := round(0); j < numRounds; j++ {
		reference := ledgers[0].(*testLedger).entries[startRound+j].Digest()
		for i := range ledgers {
			if ledgers[i].(*testLedger).entries[startRound+j].Digest() != reference {
				panic("wrong block confirmed")
			}
		}
	}
}

func simulateAgreement(t *testing.T, numNodes int, numRounds int, traceLevel traceLevel) {
	simulateAgreementWithLedgerFactory(t, numNodes, numRounds, traceLevel, makeTestLedger)
}

func simulateAgreementWithLedgerFactory(t *testing.T, numNodes int, numRounds int, traceLevel traceLevel, ledgerFactory func(map[basics.Address]basics.BalanceRecord) Ledger) {
	_, baseLedger, cleanupFn, services, clocks, ledgers, activityMonitor := setupAgreement(t, numNodes, traceLevel, ledgerFactory)
	startRound := baseLedger.NextRound()
	defer cleanupFn()

	for i := 0; i < numNodes; i++ {
		services[i].Start()
	}
	activityMonitor.waitForActivity()
	activityMonitor.waitForQuiet()
	zeroes := expectNewPeriod(clocks, 0)

	for j := 0; j < numRounds; j++ {
		zeroes = runRound(clocks, activityMonitor, zeroes)
	}

	for i := 0; i < numNodes; i++ {
		services[i].Shutdown()
	}

	sanityCheck(startRound, round(numRounds), ledgers)
}

func TestAgreementSynchronous1(t *testing.T) {
	// if testing.Short() {
	// 	t.Skip("Skipping agreement integration test")
	// }

	simulateAgreement(t, 1, 5, disabled)
}

func TestAgreementSynchronous2(t *testing.T) {
	// if testing.Short() {
	// 	t.Skip("Skipping agreement integration test")
	// }

	simulateAgreement(t, 2, 5, disabled)
}

func TestAgreementSynchronous3(t *testing.T) {
	// if testing.Short() {
	// 	t.Skip("Skipping agreement integration test")
	// }

	simulateAgreement(t, 3, 5, disabled)
}

func TestAgreementSynchronous4(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping agreement integration test")
	}

	simulateAgreement(t, 4, 5, disabled)
}

func TestAgreementSynchronous5(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping agreement integration test")
	}

	simulateAgreement(t, 5, 5, disabled)
}

func TestAgreementSynchronous10(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping agreement integration test")
	}

	simulateAgreement(t, 10, 5, disabled)
}

func TestAgreementSynchronous5_50(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping agreement integration test")
	}

	simulateAgreement(t, 5, 50, disabled)
}

func TestAgreementFastRecoveryDownEarly(t *testing.T) {
	numNodes := 5
	baseNetwork, baseLedger, cleanupFn, services, clocks, ledgers, activityMonitor := setupAgreement(t, numNodes, disabled, makeTestLedger)
	startRound := baseLedger.NextRound()
	defer cleanupFn()

	for i := 0; i < numNodes; i++ {
		services[i].Start()
	}
	activityMonitor.waitForActivity()
	activityMonitor.waitForQuiet()
	zeroes := expectNewPeriod(clocks, 0)

	// run two rounds
	for j := 0; j < 2; j++ {
		zeroes = runRound(clocks, activityMonitor, zeroes)
	}

	// force fast partition recovery into bottom
	{
		baseNetwork.dropAllSoftVotes()
		baseNetwork.dropAllSlowNextVotes()
		triggerGlobalTimeout(filterTimeout, clocks, activityMonitor)
		zeroes = expectNoNewPeriod(clocks, zeroes)

		triggerGlobalTimeout(deadlineTimeout, clocks, activityMonitor)
		zeroes = expectNoNewPeriod(clocks, zeroes)

		triggerGlobalTimeout(0, clocks, activityMonitor) // activates fast partition recovery timer
		zeroes = expectNoNewPeriod(clocks, zeroes)

		triggerGlobalTimeout(firstFPR, clocks, activityMonitor)
		zeroes = expectNewPeriod(clocks, zeroes)
	}

	// terminate on period 1
	{
		baseNetwork.repairAll()
		triggerGlobalTimeout(filterTimeout, clocks, activityMonitor)
		zeroes = expectNewPeriod(clocks, zeroes)
	}

	// run two more rounds
	for j := 0; j < 2; j++ {
		zeroes = runRound(clocks, activityMonitor, zeroes)
	}

	for i := 0; i < numNodes; i++ {
		services[i].Shutdown()
	}

	sanityCheck(startRound, 5, ledgers)
}

func TestAgreementFastRecoveryDownMiss(t *testing.T) {
	numNodes := 5
	baseNetwork, baseLedger, cleanupFn, services, clocks, ledgers, activityMonitor := setupAgreement(t, numNodes, disabled, makeTestLedger)
	startRound := baseLedger.NextRound()
	defer cleanupFn()

	for i := 0; i < numNodes; i++ {
		services[i].Start()
	}
	activityMonitor.waitForActivity()
	activityMonitor.waitForQuiet()
	zeroes := expectNewPeriod(clocks, 0)

	// run two rounds
	for j := 0; j < 2; j++ {
		zeroes = runRound(clocks, activityMonitor, zeroes)
	}

	// force fast partition recovery into bottom
	{
		// fail all steps
		baseNetwork.dropAllVotes()
		triggerGlobalTimeout(filterTimeout, clocks, activityMonitor)
		zeroes = expectNoNewPeriod(clocks, zeroes)

		triggerGlobalTimeout(deadlineTimeout, clocks, activityMonitor)
		zeroes = expectNoNewPeriod(clocks, zeroes)

		triggerGlobalTimeout(0, clocks, activityMonitor) // activates fast partition recovery timer
		zeroes = expectNoNewPeriod(clocks, zeroes)

		firstClocks := clocks[:4]
		restClocks := clocks[4:]

		for i := range firstClocks {
			firstClocks[i].(*testingClock).prepareToFire()
		}
		for i := range firstClocks {
			firstClocks[i].(*testingClock).fire(firstFPR)
		}
		activityMonitor.waitForActivity()
		activityMonitor.waitForQuiet()
		zeroes = expectNoNewPeriod(clocks, zeroes)

		baseNetwork.repairAll()
		for i := range restClocks {
			restClocks[i].(*testingClock).prepareToFire()
		}
		for i := range restClocks {
			restClocks[i].(*testingClock).fire(firstFPR)
		}
		activityMonitor.waitForActivity()
		activityMonitor.waitForQuiet()
		zeroes = expectNoNewPeriod(clocks, zeroes)

		triggerGlobalTimeout(secondFPR, clocks, activityMonitor)
		zeroes = expectNewPeriod(clocks, zeroes)
	}

	// terminate on period 1
	{
		baseNetwork.repairAll()
		triggerGlobalTimeout(filterTimeout, clocks, activityMonitor)
		zeroes = expectNewPeriod(clocks, zeroes)
	}

	// run two more rounds
	for j := 0; j < 2; j++ {
		zeroes = runRound(clocks, activityMonitor, zeroes)
	}

	for i := 0; i < numNodes; i++ {
		services[i].Shutdown()
	}

	sanityCheck(startRound, 5, ledgers)
}

func TestAgreementFastRecoveryLate(t *testing.T) {
	numNodes := 5
	baseNetwork, baseLedger, cleanupFn, services, clocks, ledgers, activityMonitor := setupAgreement(t, numNodes, disabled, makeTestLedger)
	startRound := baseLedger.NextRound()
	defer cleanupFn()

	for i := 0; i < numNodes; i++ {
		services[i].Start()
	}
	activityMonitor.waitForActivity()
	activityMonitor.waitForQuiet()
	zeroes := expectNewPeriod(clocks, 0)

	// run two rounds
	for j := 0; j < 2; j++ {
		zeroes = runRound(clocks, activityMonitor, zeroes)
	}

	// force fast partition recovery into value
	var expected proposalValue
	{
		pocket := make(chan multicastParams, 100)
		closeFn := baseNetwork.pocketAllCertVotes(pocket)
		baseNetwork.dropAllSlowNextVotes()
		triggerGlobalTimeout(filterTimeout, clocks, activityMonitor)
		zeroes = expectNoNewPeriod(clocks, zeroes)
		closeFn()

		for msg := range pocket {
			var uv unauthenticatedVote
			err := protocol.DecodeStream(bytes.NewBuffer(msg.data), &uv)
			if err != nil {
				panic(err)
			}

			if expected == (proposalValue{}) {
				expected = uv.R.Proposal
			} else {
				if uv.R.Proposal != expected {
					errstr := fmt.Sprintf("got unexpected proposal: %v != %v", uv.R.Proposal, expected)
					panic(errstr)
				}
			}
		}

		triggerGlobalTimeout(deadlineTimeout, clocks, activityMonitor)
		zeroes = expectNoNewPeriod(clocks, zeroes)

		triggerGlobalTimeout(0, clocks, activityMonitor) // activates fast partition recovery timer
		zeroes = expectNoNewPeriod(clocks, zeroes)
		baseNetwork.dropAllVotes()

		firstClocks := clocks[:4]
		restClocks := clocks[4:]

		for i := range firstClocks {
			firstClocks[i].(*testingClock).prepareToFire()
		}
		for i := range firstClocks {
			firstClocks[i].(*testingClock).fire(firstFPR)
		}
		activityMonitor.waitForActivity()
		activityMonitor.waitForQuiet()
		zeroes = expectNoNewPeriod(clocks, zeroes)

		baseNetwork.repairAll()
		for i := range restClocks {
			restClocks[i].(*testingClock).prepareToFire()
		}
		for i := range restClocks {
			restClocks[i].(*testingClock).fire(firstFPR)
		}
		activityMonitor.waitForActivity()
		activityMonitor.waitForQuiet()
		zeroes = expectNoNewPeriod(clocks, zeroes)

		triggerGlobalTimeout(secondFPR, clocks, activityMonitor)
		zeroes = expectNewPeriod(clocks, zeroes)
	}

	// terminate on period 1
	{
		baseNetwork.repairAll()
		triggerGlobalTimeout(filterTimeout, clocks, activityMonitor)
		zeroes = expectNewPeriod(clocks, zeroes)
	}

	for _, l := range ledgers {
		lastHash, err := l.LookupDigest(l.NextRound() - 1)
		if err != nil {
			panic(err)
		}
		if lastHash != expected.BlockDigest {
			errstr := fmt.Sprintf("converged on wrong block: %v != %v", lastHash, expected.BlockDigest)
			panic(errstr)
		}
	}

	// run two more rounds
	for j := 0; j < 2; j++ {
		zeroes = runRound(clocks, activityMonitor, zeroes)
	}

	for i := 0; i < numNodes; i++ {
		services[i].Shutdown()
	}

	sanityCheck(startRound, 5, ledgers)
}

func TestAgreementFastRecoveryRedo(t *testing.T) {
	numNodes := 5
	baseNetwork, baseLedger, cleanupFn, services, clocks, ledgers, activityMonitor := setupAgreement(t, numNodes, disabled, makeTestLedger)
	startRound := baseLedger.NextRound()
	defer cleanupFn()

	for i := 0; i < numNodes; i++ {
		services[i].Start()
	}
	activityMonitor.waitForActivity()
	activityMonitor.waitForQuiet()
	zeroes := expectNewPeriod(clocks, 0)

	// run two rounds
	for j := 0; j < 2; j++ {
		zeroes = runRound(clocks, activityMonitor, zeroes)
	}

	// force fast partition recovery into value
	var expected proposalValue
	{
		pocket := make(chan multicastParams, 100)
		closeFn := baseNetwork.pocketAllCertVotes(pocket)
		baseNetwork.dropAllSlowNextVotes()
		triggerGlobalTimeout(filterTimeout, clocks, activityMonitor)
		zeroes = expectNoNewPeriod(clocks, zeroes)
		closeFn()

		for msg := range pocket {
			var uv unauthenticatedVote
			err := protocol.DecodeStream(bytes.NewBuffer(msg.data), &uv)
			if err != nil {
				panic(err)
			}

			if expected == (proposalValue{}) {
				expected = uv.R.Proposal
			} else {
				if uv.R.Proposal != expected {
					errstr := fmt.Sprintf("got unexpected proposal: %v != %v", uv.R.Proposal, expected)
					panic(errstr)
				}
			}
		}

		triggerGlobalTimeout(deadlineTimeout, clocks, activityMonitor)
		zeroes = expectNoNewPeriod(clocks, zeroes)

		triggerGlobalTimeout(0, clocks, activityMonitor) // activates fast partition recovery timer
		zeroes = expectNoNewPeriod(clocks, zeroes)
		baseNetwork.dropAllVotes()

		firstClocks := clocks[:4]
		restClocks := clocks[4:]

		for i := range firstClocks {
			firstClocks[i].(*testingClock).prepareToFire()
		}
		for i := range firstClocks {
			firstClocks[i].(*testingClock).fire(firstFPR)
		}
		activityMonitor.waitForActivity()
		activityMonitor.waitForQuiet()
		zeroes = expectNoNewPeriod(clocks, zeroes)

		baseNetwork.repairAll()
		for i := range restClocks {
			restClocks[i].(*testingClock).prepareToFire()
		}
		for i := range restClocks {
			restClocks[i].(*testingClock).fire(firstFPR)
		}
		activityMonitor.waitForActivity()
		activityMonitor.waitForQuiet()
		zeroes = expectNoNewPeriod(clocks, zeroes)

		triggerGlobalTimeout(secondFPR, clocks, activityMonitor)
		zeroes = expectNewPeriod(clocks, zeroes)
	}

	// fail period 1 with value again
	{
		baseNetwork.dropAllVotes()
		triggerGlobalTimeout(filterTimeout, clocks, activityMonitor)
		zeroes = expectNoNewPeriod(clocks, zeroes)

		triggerGlobalTimeout(deadlineTimeout, clocks, activityMonitor)
		zeroes = expectNoNewPeriod(clocks, zeroes)

		triggerGlobalTimeout(0, clocks, activityMonitor) // activates fast partition recovery timer
		zeroes = expectNoNewPeriod(clocks, zeroes)
		baseNetwork.dropAllVotes()

		firstClocks := clocks[:4]
		restClocks := clocks[4:]

		for i := range firstClocks {
			firstClocks[i].(*testingClock).prepareToFire()
		}
		for i := range firstClocks {
			firstClocks[i].(*testingClock).fire(firstFPR)
		}
		activityMonitor.waitForActivity()
		activityMonitor.waitForQuiet()
		zeroes = expectNoNewPeriod(clocks, zeroes)

		baseNetwork.repairAll()
		for i := range restClocks {
			restClocks[i].(*testingClock).prepareToFire()
		}
		for i := range restClocks {
			restClocks[i].(*testingClock).fire(firstFPR)
		}
		activityMonitor.waitForActivity()
		activityMonitor.waitForQuiet()
		zeroes = expectNoNewPeriod(clocks, zeroes)

		triggerGlobalTimeout(secondFPR, clocks, activityMonitor)
		zeroes = expectNewPeriod(clocks, zeroes)
	}

	// terminate on period 2
	{
		baseNetwork.repairAll()
		triggerGlobalTimeout(filterTimeout, clocks, activityMonitor)
		zeroes = expectNewPeriod(clocks, zeroes)
	}

	for _, l := range ledgers {
		lastHash, err := l.LookupDigest(l.NextRound() - 1)
		if err != nil {
			panic(err)
		}
		if lastHash != expected.BlockDigest {
			errstr := fmt.Sprintf("converged on wrong block: %v != %v", lastHash, expected.BlockDigest)
			panic(errstr)
		}
	}

	// run two more rounds
	for j := 0; j < 2; j++ {
		zeroes = runRound(clocks, activityMonitor, zeroes)
	}

	for i := 0; i < numNodes; i++ {
		services[i].Shutdown()
	}

	sanityCheck(startRound, 5, ledgers)
}

func TestAgreementBlockReplayBug_b29ea57(t *testing.T) {
	numNodes := 2
	baseNetwork, baseLedger, cleanupFn, services, clocks, ledgers, activityMonitor := setupAgreement(t, numNodes, disabled, makeTestLedger)
	startRound := baseLedger.NextRound()
	defer cleanupFn()

	for i := 0; i < numNodes; i++ {
		services[i].Start()
	}
	activityMonitor.waitForActivity()
	activityMonitor.waitForQuiet()
	zeroes := expectNewPeriod(clocks, 0)

	// run two rounds
	for j := 0; j < 2; j++ {
		zeroes = runRound(clocks, activityMonitor, zeroes)
	}

	// fail period 0
	{
		baseNetwork.dropAllSoftVotes()
		triggerGlobalTimeout(filterTimeout, clocks, activityMonitor)
		zeroes = expectNoNewPeriod(clocks, zeroes)

		triggerGlobalTimeout(deadlineTimeout, clocks, activityMonitor)
		zeroes = expectNewPeriod(clocks, zeroes)
	}

	// fail period 1 on bottom with block
	{
		triggerGlobalTimeout(filterTimeout, clocks, activityMonitor)
		zeroes = expectNoNewPeriod(clocks, zeroes)

		triggerGlobalTimeout(deadlineTimeout, clocks, activityMonitor)
		zeroes = expectNewPeriod(clocks, zeroes)
	}

	// terminate on period 2
	{
		baseNetwork.repairAll()
		triggerGlobalTimeout(filterTimeout, clocks, activityMonitor)
		zeroes = expectNewPeriod(clocks, zeroes)
	}

	// run two more rounds
	for j := 0; j < 2; j++ {
		zeroes = runRound(clocks, activityMonitor, zeroes)
	}

	for i := 0; i < numNodes; i++ {
		services[i].Shutdown()
	}

	sanityCheck(startRound, 5, ledgers)
}

func TestAgreementLateCertBug(t *testing.T) {
	numNodes := 5
	baseNetwork, baseLedger, cleanupFn, services, clocks, ledgers, activityMonitor := setupAgreement(t, numNodes, disabled, makeTestLedger)
	startRound := baseLedger.NextRound()
	defer cleanupFn()

	for i := 0; i < numNodes; i++ {
		services[i].Start()
	}
	activityMonitor.waitForActivity()
	activityMonitor.waitForQuiet()
	zeroes := expectNewPeriod(clocks, 0)

	// run two rounds
	for j := 0; j < 2; j++ {
		zeroes = runRound(clocks, activityMonitor, zeroes)
	}

	// delay minority cert votes to force period 1
	pocket := make(chan multicastParams, 100)
	{
		closeFn := baseNetwork.pocketAllCertVotes(pocket)
		triggerGlobalTimeout(filterTimeout, clocks, activityMonitor)
		zeroes = expectNoNewPeriod(clocks, zeroes)
		closeFn()
		baseNetwork.repairAll()

		triggerGlobalTimeout(deadlineTimeout, clocks, activityMonitor)
		zeroes = expectNewPeriod(clocks, zeroes)
	}

	// terminate on period 0 in period 1
	{
		baseNetwork.prepareAllMulticast()
		for p := range pocket {
			baseNetwork.multicast(p.tag, p.data, p.source, p.exclude)
		}
		baseNetwork.finishAllMulticast()
		activityMonitor.waitForActivity()
		activityMonitor.waitForQuiet()
		zeroes = expectNewPeriod(clocks, zeroes)
	}

	// run two more rounds
	for j := 0; j < 2; j++ {
		zeroes = runRound(clocks, activityMonitor, zeroes)
	}

	for i := 0; i < numNodes; i++ {
		services[i].Shutdown()
	}

	sanityCheck(startRound, 5, ledgers)
}

func TestAgreementRecoverGlobalStartingValue(t *testing.T) {
	numNodes := 5
	baseNetwork, baseLedger, cleanupFn, services, clocks, ledgers, activityMonitor := setupAgreement(t, numNodes, disabled, makeTestLedger)
	startRound := baseLedger.NextRound()
	defer cleanupFn()

	for i := 0; i < numNodes; i++ {
		services[i].Start()
	}
	activityMonitor.waitForActivity()
	activityMonitor.waitForQuiet()
	zeroes := expectNewPeriod(clocks, 0)

	// run two rounds
	for j := 0; j < 2; j++ {
		zeroes = runRound(clocks, activityMonitor, zeroes)
	}

	// force partition recovery into value
	var expected proposalValue
	{
		pocket := make(chan multicastParams, 100)
		closeFn := baseNetwork.pocketAllCertVotes(pocket)

		triggerGlobalTimeout(filterTimeout, clocks, activityMonitor)
		zeroes = expectNoNewPeriod(clocks, zeroes)
		closeFn()

		for msg := range pocket {
			var uv unauthenticatedVote
			err := protocol.DecodeStream(bytes.NewBuffer(msg.data), &uv)
			if err != nil {
				panic(err)
			}

			if expected == (proposalValue{}) {
				expected = uv.R.Proposal
			} else {
				if uv.R.Proposal != expected {
					errstr := fmt.Sprintf("got unexpected proposal: %v != %v", uv.R.Proposal, expected)
					panic(errstr)
				}
			}
		}

		triggerGlobalTimeout(deadlineTimeout, clocks, activityMonitor)
		zeroes = expectNewPeriod(clocks, zeroes)
		require.Equal(t, 4, int(zeroes))
	}

	// now, enter period 1; check that the pocket cert is for the same value
	{
		pocket := make(chan multicastParams, 100)
		closeFn := baseNetwork.pocketAllCertVotes(pocket)

		triggerGlobalTimeout(filterTimeout, clocks, activityMonitor)
		zeroes = expectNoNewPeriod(clocks, zeroes)
		closeFn()

		for msg := range pocket {
			var uv unauthenticatedVote
			err := protocol.DecodeStream(bytes.NewBuffer(msg.data), &uv)
			if err != nil {
				panic(err)
			}

			if uv.R.Proposal != expected {
				errstr := fmt.Sprintf("got unexpected proposal: %v != %v", uv.R.Proposal, expected)
				panic(errstr)
			}
		}

		triggerGlobalTimeout(deadlineTimeout, clocks, activityMonitor)
		zeroes = expectNewPeriod(clocks, zeroes)
		require.Equal(t, 5, int(zeroes))
	}

	// now, enter period 2, and ensure agreement.
	// todo: make more transparent, I want to kow what v we agreed on
	{
		baseNetwork.repairAll()
		triggerGlobalTimeout(filterTimeout, clocks, activityMonitor)
		zeroes = expectNewPeriod(clocks, zeroes)
		require.Equal(t, 6, int(zeroes))
	}

	// run two more rounds
	for j := 0; j < 2; j++ {
		zeroes = runRound(clocks, activityMonitor, zeroes)
	}
	for i := 0; i < numNodes; i++ {
		services[i].Shutdown()
	}

	sanityCheck(startRound, 5, ledgers)
}

func TestAgreementRecoverGlobalStartingValueBadProposal(t *testing.T) {
	numNodes := 5
	baseNetwork, baseLedger, cleanupFn, services, clocks, ledgers, activityMonitor := setupAgreement(t, numNodes, disabled, makeTestLedger)
	startRound := baseLedger.NextRound()
	defer cleanupFn()

	for i := 0; i < numNodes; i++ {
		services[i].Start()
	}
	activityMonitor.waitForActivity()
	activityMonitor.waitForQuiet()
	zeroes := expectNewPeriod(clocks, 0)

	// run two rounds
	for j := 0; j < 2; j++ {
		zeroes = runRound(clocks, activityMonitor, zeroes)
	}

	// force partition recovery into value.
	var expected proposalValue
	{
		pocket := make(chan multicastParams, 100)
		closeFn := baseNetwork.pocketAllCertVotes(pocket)
		triggerGlobalTimeout(filterTimeout, clocks, activityMonitor)
		zeroes = expectNoNewPeriod(clocks, zeroes)
		closeFn()

		for msg := range pocket {
			var uv unauthenticatedVote
			err := protocol.DecodeStream(bytes.NewBuffer(msg.data), &uv)
			if err != nil {
				panic(err)
			}

			if expected == (proposalValue{}) {
				expected = uv.R.Proposal
			} else {
				if uv.R.Proposal != expected {
					errstr := fmt.Sprintf("got unexpected proposal: %v != %v", uv.R.Proposal, expected)
					panic(errstr)
				}
			}
		}
		// intercept all proposals for the next period; replace with unexpected
		baseNetwork.intercept(func(params multicastParams) multicastParams {
			if params.tag == protocol.ProposalPayloadTag {
				params.tag = protocol.UnknownMsgTag
			}
			return params
		})
		triggerGlobalTimeout(deadlineTimeout, clocks, activityMonitor)
		zeroes = expectNewPeriod(clocks, zeroes)
		require.Equal(t, 4, int(zeroes))
	}

	// Now, try again in period 1. Bad proposal should not make it and starting value should be preserved
	{
		baseNetwork.repairAll()
		pocket := make(chan multicastParams, 100)
		closeFn := baseNetwork.pocketAllCertVotes(pocket)
		triggerGlobalTimeout(filterTimeout, clocks, activityMonitor)
		zeroes = expectNoNewPeriod(clocks, zeroes)
		closeFn()

		for msg := range pocket {
			var uv unauthenticatedVote
			err := protocol.DecodeStream(bytes.NewBuffer(msg.data), &uv)
			if err != nil {
				panic(err)
			}

			if uv.R.Proposal != expected {
				errstr := fmt.Sprintf("got unexpected proposal: %v != %v", uv.R.Proposal, expected)
				panic(errstr)
			}
		}
		triggerGlobalTimeout(deadlineTimeout, clocks, activityMonitor)
		zeroes = expectNewPeriod(clocks, zeroes)

	}

	// Finish in period 2
	{
		baseNetwork.repairAll()
		triggerGlobalTimeout(filterTimeout, clocks, activityMonitor)
		zeroes = expectNewPeriod(clocks, zeroes)
		require.Equal(t, 6, int(zeroes))
	}

	// run two more rounds
	for j := 0; j < 2; j++ {
		zeroes = runRound(clocks, activityMonitor, zeroes)
	}
	for i := 0; i < numNodes; i++ {
		services[i].Shutdown()
	}

	sanityCheck(startRound, 5, ledgers)
}

func TestAgreementRecoverBothVAndBotQuorums(t *testing.T) {
	numNodes := 5
	baseNetwork, baseLedger, cleanupFn, services, clocks, ledgers, activityMonitor := setupAgreement(t, numNodes, disabled, makeTestLedger)
	startRound := baseLedger.NextRound()
	defer cleanupFn()

	for i := 0; i < numNodes; i++ {
		services[i].Start()
	}
	activityMonitor.waitForActivity()
	activityMonitor.waitForQuiet()
	zeroes := expectNewPeriod(clocks, 0)

	// run two rounds
	for j := 0; j < 2; j++ {
		zeroes = runRound(clocks, activityMonitor, zeroes)
	}

	// force partition recovery into both bottom and value. one node enters bottom, the rest enter value
	var expected proposalValue
	{
		pocket := make(chan multicastParams, 100)
		closeFn := baseNetwork.pocketAllSoftVotes(pocket)
		triggerGlobalTimeout(filterTimeout, clocks, activityMonitor)
		zeroes = expectNoNewPeriod(clocks, zeroes)
		closeFn()
		pocketedSoft := make([]multicastParams, len(pocket))
		i := 0
		for params := range pocket {
			r := bytes.NewBuffer(params.data)
			var uv unauthenticatedVote
			err := protocol.DecodeStream(r, &uv)
			if err != nil {
				panic(err)
			}
			if expected == (proposalValue{}) {
				expected = uv.R.Proposal
			} else {
				if uv.R.Proposal != expected {
					errstr := fmt.Sprintf("got unexpected soft vote: %v != %v", uv.R.Proposal, expected)
					panic(errstr)
				}
			}
			pocketedSoft[i] = params
			i++
		}
		// generate a bottom quorum; let only one node see it.
		baseNetwork.crown(0)
		triggerGlobalTimeout(deadlineTimeout, clocks, activityMonitor)
		if clocks[0].(*testingClock).zeroes != zeroes+1 {
			errstr := fmt.Sprintf("node 0 did not enter new period from bot quorum")
			panic(errstr)
		}
		zeroes = expectNoNewPeriod(clocks[1:], zeroes)

		// enable creation of a value quorum; let everyone else see it
		baseNetwork.repairAll()
		baseNetwork.prepareAllMulticast()
		for _, p := range pocketedSoft {
			baseNetwork.multicast(p.tag, p.data, p.source, p.exclude)
		}
		baseNetwork.finishAllMulticast()
		activityMonitor.waitForActivity()
		activityMonitor.waitForQuiet()

		// actually create the value quorum
		_, upper := (next).nextVoteRanges()
		triggerGlobalTimeout(upper, clocks[1:], activityMonitor) // activates next timers
		zeroes = expectNoNewPeriod(clocks[1:], zeroes)

		lower, upper := (next + 1).nextVoteRanges()
		delta := time.Duration(testingRand{}.Uint64() % uint64(upper-lower))
		triggerGlobalTimeout(lower+delta, clocks[1:], activityMonitor)
		zeroes = expectNewPeriod(clocks, zeroes)
		require.Equal(t, 4, int(zeroes))
	}

	// Now, try again in period 1. We should vote on reproposal due to non-propagation of bottom bundle.
	{
		baseNetwork.repairAll()
		pocket := make(chan multicastParams, 100)
		closeFn := baseNetwork.pocketAllCertVotes(pocket)
		triggerGlobalTimeout(filterTimeout, clocks, activityMonitor)
		zeroes = expectNoNewPeriod(clocks, zeroes)
		closeFn()

		for msg := range pocket {
			var uv unauthenticatedVote
			err := protocol.DecodeStream(bytes.NewBuffer(msg.data), &uv)
			if err != nil {
				panic(err)
			}

			if uv.R.Proposal != expected {
				errstr := fmt.Sprintf("got unexpected proposal: %v != %v", uv.R.Proposal, expected)
				panic(errstr)
			}
		}

		triggerGlobalTimeout(deadlineTimeout, clocks, activityMonitor)
		zeroes = expectNewPeriod(clocks, zeroes)
	}

	// Finish in period 2
	{
		baseNetwork.repairAll()
		triggerGlobalTimeout(filterTimeout, clocks, activityMonitor)
		zeroes = expectNewPeriod(clocks, zeroes)
		require.Equal(t, 6, int(zeroes))
	}

	// run two more rounds
	for j := 0; j < 2; j++ {
		zeroes = runRound(clocks, activityMonitor, zeroes)
	}
	for i := 0; i < numNodes; i++ {
		services[i].Shutdown()
	}

	sanityCheck(startRound, 5, ledgers)
}

func TestAgreementSlowPayloadsPreDeadline(t *testing.T) {
	numNodes := 5
	baseNetwork, baseLedger, cleanupFn, services, clocks, ledgers, activityMonitor := setupAgreement(t, numNodes, disabled, makeTestLedger)
	startRound := baseLedger.NextRound()
	defer cleanupFn()

	for i := 0; i < numNodes; i++ {
		services[i].Start()
	}
	activityMonitor.waitForActivity()
	activityMonitor.waitForQuiet()
	zeroes := expectNewPeriod(clocks, 0)

	// run two rounds
	for j := 0; j < 2; j++ {
		zeroes = runRound(clocks, activityMonitor, zeroes)
	}

	// run round and then start pocketing payloads
	pocket := make(chan multicastParams, 100)
	closeFn := baseNetwork.pocketAllCompound(pocket) // (takes effect next round)
	{
		triggerGlobalTimeout(filterTimeout, clocks, activityMonitor)
		zeroes = expectNewPeriod(clocks, zeroes)
	}

	// run round with late payload
	{
		triggerGlobalTimeout(filterTimeout, clocks, activityMonitor)
		zeroes = expectNoNewPeriod(clocks, zeroes)

		// release payloads; expect new round
		closeFn()
		baseNetwork.repairAll()
		baseNetwork.prepareAllMulticast()
		for p := range pocket {
			baseNetwork.multicast(p.tag, p.data, p.source, p.exclude)
		}
		baseNetwork.finishAllMulticast()
		activityMonitor.waitForActivity()
		activityMonitor.waitForQuiet()
		zeroes = expectNewPeriod(clocks, zeroes)
	}

	// run two more rounds
	for j := 0; j < 2; j++ {
		zeroes = runRound(clocks, activityMonitor, zeroes)
	}
	for i := 0; i < numNodes; i++ {
		services[i].Shutdown()
	}

	sanityCheck(startRound, 6, ledgers)
}

func TestAgreementSlowPayloadsPostDeadline(t *testing.T) {
	numNodes := 5
	baseNetwork, baseLedger, cleanupFn, services, clocks, ledgers, activityMonitor := setupAgreement(t, numNodes, disabled, makeTestLedger)
	startRound := baseLedger.NextRound()
	defer cleanupFn()

	for i := 0; i < numNodes; i++ {
		services[i].Start()
	}
	activityMonitor.waitForActivity()
	activityMonitor.waitForQuiet()
	zeroes := expectNewPeriod(clocks, 0)

	// run two rounds
	for j := 0; j < 2; j++ {
		zeroes = runRound(clocks, activityMonitor, zeroes)
	}

	// run round and then start pocketing payloads
	pocket := make(chan multicastParams, 100)
	closeFn := baseNetwork.pocketAllCompound(pocket) // (takes effect next round)
	{
		triggerGlobalTimeout(filterTimeout, clocks, activityMonitor)
		zeroes = expectNewPeriod(clocks, zeroes)
	}

	// force network into period 1 by delaying proposals
	{
		triggerGlobalTimeout(filterTimeout, clocks, activityMonitor)
		zeroes = expectNoNewPeriod(clocks, zeroes)
		triggerGlobalTimeout(deadlineTimeout, clocks, activityMonitor)
		zeroes = expectNewPeriod(clocks, zeroes)
	}

	// recover in period 1
	{
		closeFn()
		baseNetwork.repairAll()
		baseNetwork.prepareAllMulticast()
		for p := range pocket {
			baseNetwork.multicast(p.tag, p.data, p.source, p.exclude)
		}
		baseNetwork.finishAllMulticast()
		activityMonitor.waitForActivity()
		activityMonitor.waitForQuiet()
		zeroes = expectNoNewPeriod(clocks, zeroes)

		triggerGlobalTimeout(filterTimeout, clocks, activityMonitor)
		zeroes = expectNewPeriod(clocks, zeroes)
	}

	// run two more rounds
	for j := 0; j < 2; j++ {
		zeroes = runRound(clocks, activityMonitor, zeroes)
	}
	for i := 0; i < numNodes; i++ {
		services[i].Shutdown()
	}

	sanityCheck(startRound, 6, ledgers)
}

func TestAgreementLargePeriods(t *testing.T) {
	numNodes := 5
	baseNetwork, baseLedger, cleanupFn, services, clocks, ledgers, activityMonitor := setupAgreement(t, numNodes, disabled, makeTestLedger)
	startRound := baseLedger.NextRound()
	defer cleanupFn()
	for i := 0; i < numNodes; i++ {
		services[i].Start()
	}

	activityMonitor.waitForActivity()
	activityMonitor.waitForQuiet()
	zeroes := expectNewPeriod(clocks, 0)

	// run two rounds
	for j := 0; j < 2; j++ {
		zeroes = runRound(clocks, activityMonitor, zeroes)
	}

	// partition the network, run until period 60
	for p := 0; p < 60; p++ {
		{
			baseNetwork.partition(0, 1, 2)
			triggerGlobalTimeout(filterTimeout, clocks, activityMonitor)
			zeroes = expectNoNewPeriod(clocks, zeroes)

			baseNetwork.repairAll()
			triggerGlobalTimeout(deadlineTimeout, clocks, activityMonitor)
			zeroes = expectNewPeriod(clocks, zeroes)
			require.Equal(t, 4+p, int(zeroes))
		}
	}

	// terminate
	{
		triggerGlobalTimeout(filterTimeout, clocks, activityMonitor)
		zeroes = expectNewPeriod(clocks, zeroes)
	}

	// run two more rounds
	for j := 0; j < 2; j++ {
		zeroes = runRound(clocks, activityMonitor, zeroes)
	}
	for i := 0; i < numNodes; i++ {
		services[i].Shutdown()
	}

	const expectNumRounds = 5
	for i := 0; i < numNodes; i++ {
		if ledgers[i].NextRound() != startRound+round(expectNumRounds) {
			panic("did not progress 5 rounds")
		}
	}

	for j := 0; j < expectNumRounds; j++ {
		ledger := ledgers[0].(*testLedger)
		reference := ledger.entries[startRound+round(j)].Digest()
		for i := 0; i < numNodes; i++ {
			ledger := ledgers[i].(*testLedger)
			if ledger.entries[startRound+round(j)].Digest() != reference {
				panic("wrong block confirmed")
			}
		}
	}
}
