package introduce

import (
	"errors"
	"fmt"
	"math/rand"
	"testing"

	"github.com/drausin/libri/libri/common/ecid"
	"github.com/drausin/libri/libri/common/id"
	"github.com/drausin/libri/libri/librarian/api"
	lclient "github.com/drausin/libri/libri/librarian/client"
	"github.com/drausin/libri/libri/librarian/server/peer"
	"github.com/drausin/libri/libri/librarian/server/search"
	"github.com/stretchr/testify/assert"
	"golang.org/x/net/context"
	"google.golang.org/grpc"
)

func TestNewDefaultIntroducer(t *testing.T) {
	rng := rand.New(rand.NewSource(0))
	s := NewDefaultIntroducer(
		lclient.NewSigner(ecid.NewPseudoRandom(rng).Key()),
		id.NewPseudoRandom(rng),
	)
	assert.NotNil(t, s.(*introducer).signer)
	assert.NotNil(t, s.(*introducer).introducerCreator)
	assert.NotNil(t, s.(*introducer).repProcessor)
}

type fixedIntroQuerier struct{}

func (q *fixedIntroQuerier) Query(ctx context.Context, pConn peer.Connector,
	rq *api.IntroduceRequest, opts ...grpc.CallOption) (*api.IntroduceResponse, error) {
	return &api.IntroduceResponse{
		Metadata: &api.ResponseMetadata{
			RequestId: rq.Metadata.RequestId,
		},
		Self:  pConn.(*peer.TestConnector).APISelf,
		Peers: pConn.(*peer.TestConnector).Addresses,
	}, nil
}

func TestIntroducer_Introduce_ok(t *testing.T) {
	for concurrency := uint(1); concurrency <= 3; concurrency++ {
		introducer, intro, selfPeerIdxs, peers := newTestIntros(concurrency)
		seeds := search.NewTestSeeds(peers, selfPeerIdxs[:3])

		// do the intro!
		err := introducer.Introduce(intro, seeds)

		// checks
		assert.Nil(t, err)
		assert.True(t, intro.Finished())
		assert.True(t, intro.ReachedTarget())
		assert.False(t, intro.Exhausted())
		assert.False(t, intro.Errored())
		assert.Equal(t, uint(0), intro.Result.NErrors)
		assert.True(t, len(intro.Result.Responded) >=
			int(intro.Params.TargetNumIntroductions))

		// make sure at least one of the seeds has been removed from the unqueried map
		nUnqueriedSeeds := 0
		for i := range seeds {
			if _, in := intro.Result.Unqueried[fmt.Sprintf("seed%02d", i)]; in {
				nUnqueriedSeeds++
			}
		}
		assert.True(t, len(seeds) > nUnqueriedSeeds)
	}
}

func TestIntroducer_Introduce_queryErr(t *testing.T) {
	introducerImpl, intro, selfPeerIdxs, peers := newTestIntros(1)
	seeds := search.NewTestSeeds(peers, selfPeerIdxs)

	// all queries return errors as if they'd timed out
	introducerImpl.(*introducer).introducerCreator = &fixedIntroducerCreator{
		err: errors.New("some Create error"),
	}

	// do the intro!
	err := introducerImpl.Introduce(intro, seeds)

	// checks
	assert.Nil(t, err)
	assert.True(t, intro.Finished())
	assert.True(t, intro.Errored()) // since all queries returned errors
	assert.False(t, intro.Exhausted())
	assert.False(t, intro.ReachedTarget())
	assert.Equal(t, intro.Params.NMaxErrors, intro.Result.NErrors)
	assert.Nil(t, intro.Result.FatalErr)
	assert.Equal(t, 0, len(intro.Result.Responded))
}

func TestIntroducer_Introduce_rpErr(t *testing.T) {
	introducerImpl, intro, selfPeerIdxs, peers := newTestIntros(1)
	seeds := search.NewTestSeeds(peers, selfPeerIdxs)

	// mock some internal issue when processing responses
	introducerImpl.(*introducer).repProcessor = &errResponseProcessor{}

	// do the intro!
	err := introducerImpl.Introduce(intro, seeds)

	// checks
	assert.NotNil(t, err)
	assert.True(t, intro.Finished())
	assert.True(t, intro.Errored()) // since we got a fatal error while processing responses
	assert.False(t, intro.Exhausted())
	assert.False(t, intro.ReachedTarget())
	assert.NotNil(t, intro.Result.FatalErr)
	assert.Equal(t, uint(0), intro.Result.NErrors)
	assert.Equal(t, 0, len(intro.Result.Responded))
}

func TestIntroducer_query_ok(t *testing.T) {
	intro := newQueryTestIntroduction()
	introducerImpl := &introducer{
		signer:            &lclient.TestNoOpSigner{},
		introducerCreator: &fixedIntroducerCreator{},
	}

	client := &peer.TestConnector{}
	rp, err := introducerImpl.query(client, intro)

	assert.Nil(t, err)
	assert.NotNil(t, rp.Metadata.RequestId)
}

func TestIntroducer_query_err(t *testing.T) {
	clientConn := &peer.TestConnector{}
	intro := newQueryTestIntroduction()
	cases := []*introducer{
		// case 0
		{
			signer:            &lclient.TestNoOpSigner{},
			introducerCreator: &fixedIntroducerCreator{err: errors.New("some Create error")},
		},

		// case 1
		{
			signer:            &lclient.TestErrSigner{},
			introducerCreator: &fixedIntroducerCreator{},
		},

		// case 2
		{
			signer: &lclient.TestNoOpSigner{},
			introducerCreator: &fixedIntroducerCreator{
				introducer: &fixedIntroducer{err: errors.New("some Store error")},
			},
		},

		// case 3
		{
			signer: &lclient.TestNoOpSigner{},
			introducerCreator: &fixedIntroducerCreator{
				introducer: &fixedIntroducer{requestID: []byte{1, 2, 3, 4}},
			},
		},
	}
	for i, c := range cases {
		info := fmt.Sprintf("case %d", i)
		rp1, err := c.query(clientConn, intro)
		assert.Nil(t, rp1, info)
		assert.NotNil(t, err, info)
	}
}

func TestResponseProcessor_Process(t *testing.T) {
	rng := rand.New(rand.NewSource(int64(0)))
	nPeers := 16
	responder := peer.NewTestPeer(rng, nPeers)
	peers := peer.NewTestPeers(rng, nPeers)
	selfPeer := peer.NewTestPeer(rng, nPeers+1)
	rp := NewResponseProcessor(peer.NewFromer(), selfPeer.ID())
	apiPeers := peer.ToAPIs(peers)
	apiPeers = append(apiPeers, selfPeer.ToAPI())

	result := NewInitialResult()

	response1 := &api.IntroduceResponse{
		Self:  responder.ToAPI(),
		Peers: apiPeers,
	}
	err := rp.Process(response1, result)

	assert.Nil(t, err)

	// make sure we've added the responder as responded
	_, in := result.Responded[responder.ID().String()]
	assert.True(t, in)

	// make sure we've added each peer to the unqueried map
	assert.Equal(t, nPeers, len(result.Unqueried))
	for _, p := range peers {
		_, in = result.Unqueried[p.ID().String()]
		assert.True(t, in)
	}

	// make sure selfPeer isn't in the unqueried map
	_, in = result.Unqueried[selfPeer.ID().String()]
	assert.False(t, in)

	// make in identical response
	response2 := &api.IntroduceResponse{
		Self:  responder.ToAPI(),
		Peers: peer.ToAPIs(peers),
	}
	err = rp.Process(response2, result)

	assert.Nil(t, err)

	// make sure nothing has changed with Unqueried map
	assert.Equal(t, nPeers, len(result.Unqueried))
	for _, p := range peers {
		_, in = result.Unqueried[p.ID().String()]
		assert.True(t, in)
	}
}

func newQueryTestIntroduction() *Introduction {
	n, _ := 32, uint(8)
	rng := rand.New(rand.NewSource(int64(n)))
	peers, _, _, selfID := search.NewTestPeers(rng, n)
	apiSelf := &api.PeerAddress{
		PeerId: peers[0].ID().Bytes(),
	}
	intro := NewIntroduction(selfID, apiSelf, &Parameters{})
	return intro
}

type errResponseProcessor struct{}

func (erp *errResponseProcessor) Process(rp *api.IntroduceResponse, result *Result) error {
	return errors.New("some fatal processing error")
}

func newTestIntroducer(peersMap map[string]peer.Peer, selfID id.ID) Introducer {
	return NewIntroducer(
		&lclient.TestNoOpSigner{},
		&fixedIntroducerCreator{},
		&responseProcessor{
			fromer: &search.TestFromer{Peers: peersMap},
			selfID: selfID,
		},
	)
}

func newTestIntros(concurrency uint) (Introducer, *Introduction, []int, []peer.Peer) {
	n, targetNumIntros := 256, uint(64)
	rng := rand.New(rand.NewSource(int64(n)))
	peers, peersMap, selfPeerIdxs, selfID := search.NewTestPeers(rng, n)
	apiSelf := &api.PeerAddress{
		PeerId: peers[0].ID().Bytes(),
	}

	// create our introducer
	introducer := newTestIntroducer(peersMap, selfID.ID())

	intro := NewIntroduction(selfID, apiSelf, &Parameters{
		TargetNumIntroductions: targetNumIntros,
		NumPeersPerRequest:     DefaultNumPeersPerRequest,
		NMaxErrors:             DefaultNMaxErrors,
		Concurrency:            concurrency,
		Timeout:                DefaultQueryTimeout,
	})

	return introducer, intro, selfPeerIdxs, peers
}

type fixedIntroducerCreator struct {
	introducer api.Introducer
	err        error
}

func (c *fixedIntroducerCreator) Create(pConn peer.Connector) (api.Introducer, error) {
	if c.err != nil {
		return nil, c.err
	}
	if c.introducer != nil {
		return c.introducer, nil
	}
	return &fixedIntroducer{
		self:      pConn.(*peer.TestConnector).APISelf,
		addresses: pConn.(*peer.TestConnector).Addresses,
	}, nil
}

type fixedIntroducer struct {
	self      *api.PeerAddress
	addresses []*api.PeerAddress
	requestID []byte
	err       error
}

func (f *fixedIntroducer) Introduce(ctx context.Context, rq *api.IntroduceRequest,
	opts ...grpc.CallOption) (*api.IntroduceResponse, error) {

	if f.err != nil {
		return nil, f.err
	}
	requestID := f.requestID
	if requestID == nil {
		requestID = rq.Metadata.RequestId
	}
	return &api.IntroduceResponse{
		Metadata: &api.ResponseMetadata{
			RequestId: requestID,
		},
		Self:  f.self,
		Peers: f.addresses,
	}, nil
}
