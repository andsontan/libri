package search

import (
	"container/heap"
	"errors"
	"fmt"
	"math/rand"
	"testing"
	"time"

	"github.com/drausin/libri/libri/common/ecid"
	"github.com/drausin/libri/libri/common/id"
	"github.com/drausin/libri/libri/librarian/api"
	"github.com/drausin/libri/libri/librarian/client"
	"github.com/drausin/libri/libri/librarian/server/peer"
	"github.com/stretchr/testify/assert"
)

func TestNewDefaultSearcher(t *testing.T) {
	rng := rand.New(rand.NewSource(0))
	s := NewDefaultSearcher(client.NewSigner(ecid.NewPseudoRandom(rng).Key()))
	assert.NotNil(t, s.(*searcher).signer)
	assert.NotNil(t, s.(*searcher).finderCreator)
	assert.NotNil(t, s.(*searcher).rp)
}

func TestSearcher_Search_ok(t *testing.T) {
	n, nClosestResponses := 32, uint(8)
	rng := rand.New(rand.NewSource(int64(n)))
	peers, peersMap, selfPeerIdxs, selfID := NewTestPeers(rng, n)

	// create our searcher
	key := id.NewPseudoRandom(rng)
	searcher := NewTestSearcher(peersMap)

	for concurrency := uint(1); concurrency <= 3; concurrency++ {

		search := NewSearch(selfID, key, &Parameters{
			NClosestResponses: nClosestResponses,
			NMaxErrors:        DefaultNMaxErrors,
			Concurrency:       concurrency,
			Timeout:           DefaultQueryTimeout,
		})

		seeds := NewTestSeeds(peers, selfPeerIdxs)

		// do the search!
		err := searcher.Search(search, seeds)

		// checks
		assert.Nil(t, err)
		assert.True(t, search.Finished())
		assert.True(t, search.FoundClosestPeers())
		assert.False(t, search.Errored())
		assert.False(t, search.Exhausted())
		assert.Equal(t, 0, len(search.Result.Errored))
		assert.Equal(t, int(nClosestResponses), search.Result.Closest.Len())
		assert.True(t, search.Result.Closest.Len() <= len(search.Result.Responded))

		// build set of closest peers by iteratively looking at all of them
		expectedClosestsPeers := make(map[string]struct{})
		farthestCloseDist := search.Result.Closest.PeakDistance()
		for _, p := range peers {
			pDist := key.Distance(p.ID())
			if pDist.Cmp(farthestCloseDist) <= 0 {
				expectedClosestsPeers[p.ID().String()] = struct{}{}
			}
		}

		// check all closest peers are in set of peers within farther close distance to
		// the key
		for search.Result.Closest.Len() > 0 {
			p := heap.Pop(search.Result.Closest).(peer.Peer)
			_, in := expectedClosestsPeers[p.ID().String()]
			assert.True(t, in)
		}
	}
}

func TestSearcher_Search_queryErr(t *testing.T) {
	searcherImpl, search, selfPeerIdxs, peers := newTestSearch()
	seeds := NewTestSeeds(peers, selfPeerIdxs)

	// duplicate seeds so we cover branch of hitting errored peer more than once
	seeds = append(seeds, seeds[0])

	// all queries return errors
	searcherImpl.(*searcher).finderCreator = &TestFinderCreator{
		err: errors.New("some Create error"),
	}

	// do the search!
	err := searcherImpl.Search(search, seeds)

	// checks
	assert.Equal(t, ErrTooManyFindErrors, err)
	assert.True(t, search.Errored())    // since all of the queries return errors
	assert.False(t, search.Exhausted()) // since NMaxErrors < len(Unqueried)
	assert.True(t, search.Finished())
	assert.False(t, search.FoundClosestPeers())
	assert.Equal(t, int(search.Params.NMaxErrors), len(search.Result.Errored)-1)
	assert.Equal(t, 0, search.Result.Closest.Len())
	assert.True(t, 0 < search.Result.Unqueried.Len())
	assert.Equal(t, 0, len(search.Result.Responded))
}

type errResponseProcessor struct{}

func (erp *errResponseProcessor) Process(rp *api.FindResponse, result *Result) error {
	return errors.New("some fatal processing error")
}

func TestSearcher_Search_rpErr(t *testing.T) {
	searcherImpl, search, selfPeerIdxs, peers := newTestSearch()
	seeds := NewTestSeeds(peers, selfPeerIdxs)

	// mock some internal issue when processing responses
	searcherImpl.(*searcher).rp = &errResponseProcessor{}

	// do the search!
	err := searcherImpl.Search(search, seeds)

	// checks
	assert.NotNil(t, err)
	assert.NotNil(t, search.Result.FatalErr)
	assert.True(t, search.Errored()) // since we got a fatal error while processing responses
	assert.False(t, search.Exhausted())
	assert.True(t, search.Finished())
	assert.False(t, search.FoundClosestPeers())
	assert.Equal(t, 0, len(search.Result.Errored))
	assert.Equal(t, 0, search.Result.Closest.Len())
	assert.True(t, 0 < search.Result.Unqueried.Len())
	assert.Equal(t, 0, len(search.Result.Responded))
}

func newTestSearch() (Searcher, *Search, []int, []peer.Peer) {
	n, nClosestResponses := 32, uint(8)
	rng := rand.New(rand.NewSource(int64(n)))
	peers, peersMap, selfPeerIdxs, selfID := NewTestPeers(rng, n)

	// create our searcher
	key := id.NewPseudoRandom(rng)
	searcher := NewTestSearcher(peersMap)

	search := NewSearch(selfID, key, &Parameters{
		NClosestResponses: nClosestResponses,
		NMaxErrors:        DefaultNMaxErrors,
		Concurrency:       uint(1),
		Timeout:           DefaultQueryTimeout,
	})
	return searcher, search, selfPeerIdxs, peers
}

func TestSearcher_query_ok(t *testing.T) {
	rng := rand.New(rand.NewSource(int64(0)))
	peerID, key := ecid.NewPseudoRandom(rng), id.NewPseudoRandom(rng)
	search := NewSearch(peerID, key, &Parameters{})
	s := &searcher{
		signer:        &client.TestNoOpSigner{},
		finderCreator: &TestFinderCreator{},
		rp:            nil,
	}
	connClient := &peer.TestConnector{}

	rp, err := s.query(connClient, search)
	assert.Nil(t, err)
	assert.NotNil(t, rp.Metadata.RequestId)
	assert.Nil(t, rp.Value)
}

func TestSearcher_query_err(t *testing.T) {
	rng := rand.New(rand.NewSource(int64(0)))
	connClient := &peer.TestConnector{}
	peerID, key := ecid.NewPseudoRandom(rng), id.NewPseudoRandom(rng)
	search := NewSearch(peerID, key, &Parameters{Timeout: 1 * time.Second})

	cases := []*searcher{
		// case 0
		{
			signer:        &client.TestNoOpSigner{},
			finderCreator: &TestFinderCreator{err: errors.New("some create error")},
		},

		// case 1
		{
			signer:        &client.TestErrSigner{},
			finderCreator: &TestFinderCreator{},
		},

		// case 2
		{
			signer: &client.TestNoOpSigner{},
			finderCreator: &TestFinderCreator{
				finder: &fixedFinder{err: errors.New("some Find error")},
			},
		},

		// case 3
		{
			signer: &client.TestNoOpSigner{},
			finderCreator: &TestFinderCreator{
				finder: &fixedFinder{requestID: []byte{1, 2, 3, 4}},
			},
		},
	}

	for i, c := range cases {
		info := fmt.Sprintf("case %d", i)
		rp, err := c.query(connClient, search)
		assert.Nil(t, rp, info)
		assert.NotNil(t, err, info)
	}
}

func TestResponseProcessor_Process_Value(t *testing.T) {
	rng := rand.New(rand.NewSource(int64(0)))
	key := id.NewPseudoRandom(rng)
	rp := NewResponseProcessor(peer.NewFromer())
	result := NewInitialResult(key, NewDefaultParameters())

	// create response with the value
	value, _ := api.NewTestDocument(rng)
	response2 := &api.FindResponse{
		Peers: nil,
		Value: value,
	}

	// check that the result value is set
	prevUnqueriedLength := result.Unqueried.Len()
	err := rp.Process(response2, result)
	assert.Nil(t, err)
	assert.Equal(t, prevUnqueriedLength, result.Unqueried.Len())
	assert.Equal(t, value, result.Value)
}

func TestResponseProcessor_Process_Addresses(t *testing.T) {
	rng := rand.New(rand.NewSource(int64(0)))

	// create placeholder api.PeerAddresses for our mocked api.FindPeers response
	nAddresses1 := 6
	peerAddresses1 := newPeerAddresses(rng, nAddresses1)

	key := id.NewPseudoRandom(rng)
	rp := NewResponseProcessor(peer.NewFromer())
	params := NewDefaultParameters()
	result := NewInitialResult(key, params)
	result.Unqueried = newClosestPeers(key, 9)

	// create response or nAddresses and process it
	response1 := &api.FindResponse{
		Peers: peerAddresses1,
		Value: nil,
	}
	err := rp.Process(response1, result)
	assert.Nil(t, err)

	// check that all responses have gone into the unqueried heap
	assert.Equal(t, nAddresses1, result.Unqueried.Len())

	// process same response as before and check that the length of unqueried hasn't changed
	err = rp.Process(response1, result)
	assert.Nil(t, err)
	assert.Equal(t, nAddresses1, result.Unqueried.Len())

	// create new peers and add them to the closest heap (as if we'd already heard from them)
	nAddresses2 := 3
	peerAddresses2 := newPeerAddresses(rng, nAddresses2)
	peerFromer := peer.NewFromer()
	for _, pa := range peerAddresses2 {
		err = result.Closest.SafePush(peerFromer.FromAPI(pa))
		assert.Nil(t, err)
	}

	// check that a response with these peers has no effect
	response2 := &api.FindResponse{
		Peers: peerAddresses2,
		Value: nil,
	}
	err = rp.Process(response2, result)
	assert.Nil(t, err)
	assert.Equal(t, nAddresses1, result.Unqueried.Len())
	assert.Equal(t, nAddresses2, result.Closest.Len())
}

func TestResponseProcessor_Process_err(t *testing.T) {
	rng := rand.New(rand.NewSource(int64(0)))
	key := id.NewPseudoRandom(rng)
	rp := NewResponseProcessor(peer.NewFromer())
	result := NewInitialResult(key, NewDefaultParameters())

	// create a bad response with neither a value nor peer addresses
	response2 := &api.FindResponse{
		Peers: nil,
		Value: nil,
	}
	err := rp.Process(response2, result)
	assert.NotNil(t, err)
}

func newPeerAddresses(rng *rand.Rand, n int) []*api.PeerAddress {
	peerAddresses := make([]*api.PeerAddress, n)
	for i := 0; i < n; i++ {
		peerAddresses[i] = &api.PeerAddress{
			PeerId:   id.NewPseudoRandom(rng).Bytes(),
			PeerName: fmt.Sprintf("peer-%03d", i),
			Ip:       "localhost",
			Port:     uint32(20100 + i),
		}
	}
	return peerAddresses
}
