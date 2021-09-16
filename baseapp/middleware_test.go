package baseapp_test

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"math"
	"math/rand"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/gogo/protobuf/jsonpb"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	abci "github.com/tendermint/tendermint/abci/types"
	"github.com/tendermint/tendermint/libs/log"
	tmproto "github.com/tendermint/tendermint/proto/tendermint/types"
	dbm "github.com/tendermint/tm-db"

	"github.com/cosmos/cosmos-sdk/baseapp"
	"github.com/cosmos/cosmos-sdk/codec"
	"github.com/cosmos/cosmos-sdk/snapshots"
	snapshottypes "github.com/cosmos/cosmos-sdk/snapshots/types"
	"github.com/cosmos/cosmos-sdk/testutil/testdata"
	sdk "github.com/cosmos/cosmos-sdk/types"
	sdkerrors "github.com/cosmos/cosmos-sdk/types/errors"
	"github.com/cosmos/cosmos-sdk/types/tx"

	"github.com/cosmos/cosmos-sdk/x/auth/middleware"
	"github.com/cosmos/cosmos-sdk/x/auth/migrations/legacytx"
)

var (
	capKey1 = sdk.NewKVStoreKey("key1")
	capKey2 = sdk.NewKVStoreKey("key2")

	interfaceRegistry = testdata.NewTestInterfaceRegistry()
)

type paramStore struct {
	db *dbm.MemDB
}

func (ps *paramStore) Set(_ sdk.Context, key []byte, value interface{}) {
	bz, err := json.Marshal(value)
	if err != nil {
		panic(err)
	}

	ps.db.Set(key, bz)
}

func (ps *paramStore) Has(_ sdk.Context, key []byte) bool {
	ok, err := ps.db.Has(key)
	if err != nil {
		panic(err)
	}

	return ok
}

func (ps *paramStore) Get(_ sdk.Context, key []byte, ptr interface{}) {
	bz, err := ps.db.Get(key)
	if err != nil {
		panic(err)
	}

	if len(bz) == 0 {
		return
	}

	if err := json.Unmarshal(bz, ptr); err != nil {
		panic(err)
	}
}

// Simple tx with a list of Msgs.
type txTest struct {
	Msgs       []sdk.Msg
	Counter    int64
	FailOnAnte bool
	GasLimit   uint64
}

func (tx *txTest) setFailOnAnte(fail bool) {
	tx.FailOnAnte = fail
}

func (tx *txTest) setFailOnHandler(fail bool) {
	for i, msg := range tx.Msgs {
		tx.Msgs[i] = msgCounter{msg.(msgCounter).Counter, fail}
	}
}

// Implements Tx
func (tx txTest) GetMsgs() []sdk.Msg   { return tx.Msgs }
func (tx txTest) ValidateBasic() error { return nil }

// Implements GasTx
func (tx txTest) GetGas() uint64 { return tx.GasLimit }

// Implements TxWithTimeoutHeight
func (tx txTest) GetTimeoutHeight() uint64 { return 0 }

const (
	routeMsgCounter  = "msgCounter"
	routeMsgCounter2 = "msgCounter2"
	routeMsgKeyValue = "msgKeyValue"
)

// ValidateBasic() fails on negative counters.
// Otherwise it's up to the handlers
type msgCounter struct {
	Counter       int64
	FailOnHandler bool
}

// dummy implementation of proto.Message
func (msg msgCounter) Reset()         {}
func (msg msgCounter) String() string { return "TODO" }
func (msg msgCounter) ProtoMessage()  {}

// Implements Msg
func (msg msgCounter) Route() string                { return routeMsgCounter }
func (msg msgCounter) Type() string                 { return "counter1" }
func (msg msgCounter) GetSignBytes() []byte         { return nil }
func (msg msgCounter) GetSigners() []sdk.AccAddress { return nil }
func (msg msgCounter) ValidateBasic() error {
	if msg.Counter >= 0 {
		return nil
	}
	return sdkerrors.Wrap(sdkerrors.ErrInvalidSequence, "counter should be a non-negative integer")
}

// a msg we dont know how to route
type msgNoRoute struct {
	msgCounter
}

func (tx msgNoRoute) Route() string { return "noroute" }

// a msg we dont know how to decode
type msgNoDecode struct {
	msgCounter
}

func (tx msgNoDecode) Route() string { return routeMsgCounter }

// Another counter msg. Duplicate of msgCounter
type msgCounter2 struct {
	Counter int64
}

// dummy implementation of proto.Message
func (msg msgCounter2) Reset()         {}
func (msg msgCounter2) String() string { return "TODO" }
func (msg msgCounter2) ProtoMessage()  {}

// Implements Msg
func (msg msgCounter2) Route() string                { return routeMsgCounter2 }
func (msg msgCounter2) Type() string                 { return "counter2" }
func (msg msgCounter2) GetSignBytes() []byte         { return nil }
func (msg msgCounter2) GetSigners() []sdk.AccAddress { return nil }
func (msg msgCounter2) ValidateBasic() error {
	if msg.Counter >= 0 {
		return nil
	}
	return sdkerrors.Wrap(sdkerrors.ErrInvalidSequence, "counter should be a non-negative integer")
}

// A msg that sets a key/value pair.
type msgKeyValue struct {
	Key   []byte
	Value []byte
}

func (msg msgKeyValue) Reset()                       {}
func (msg msgKeyValue) String() string               { return "TODO" }
func (msg msgKeyValue) ProtoMessage()                {}
func (msg msgKeyValue) Route() string                { return routeMsgKeyValue }
func (msg msgKeyValue) Type() string                 { return "keyValue" }
func (msg msgKeyValue) GetSignBytes() []byte         { return nil }
func (msg msgKeyValue) GetSigners() []sdk.AccAddress { return nil }
func (msg msgKeyValue) ValidateBasic() error {
	if msg.Key == nil {
		return sdkerrors.Wrap(sdkerrors.ErrInvalidRequest, "key cannot be nil")
	}
	if msg.Value == nil {
		return sdkerrors.Wrap(sdkerrors.ErrInvalidRequest, "value cannot be nil")
	}
	return nil
}

// amino decode
func testTxDecoder(cdc *codec.LegacyAmino) sdk.TxDecoder {
	return func(txBytes []byte) (sdk.Tx, error) {
		var tx txTest
		if len(txBytes) == 0 {
			return nil, sdkerrors.Wrap(sdkerrors.ErrTxDecode, "tx bytes are empty")
		}

		err := cdc.Unmarshal(txBytes, &tx)
		if err != nil {
			return nil, sdkerrors.ErrTxDecode
		}

		return tx, nil
	}
}

func registerTestCodec(cdc *codec.LegacyAmino) {
	// register Tx, Msg
	sdk.RegisterLegacyAminoCodec(cdc)

	// register test types
	cdc.RegisterConcrete(&txTest{}, "cosmos-sdk/baseapp/txTest", nil)
	cdc.RegisterConcrete(&msgCounter{}, "cosmos-sdk/baseapp/msgCounter", nil)
	cdc.RegisterConcrete(&msgCounter2{}, "cosmos-sdk/baseapp/msgCounter2", nil)
	cdc.RegisterConcrete(&msgKeyValue{}, "cosmos-sdk/baseapp/msgKeyValue", nil)
	cdc.RegisterConcrete(&msgNoRoute{}, "cosmos-sdk/baseapp/msgNoRoute", nil)
}

// aminoTxEncoder creates a amino TxEncoder for testing purposes.
func aminoTxEncoder() sdk.TxEncoder {
	cdc := codec.NewLegacyAmino()
	registerTestCodec(cdc)

	return legacytx.StdTxConfig{Cdc: cdc}.TxEncoder()
}

func defaultLogger() log.Logger {
	return log.NewTMLogger(log.NewSyncWriter(os.Stdout)).With("module", "sdk/app")
}

func newBaseApp(name string, options ...func(*baseapp.BaseApp)) *baseapp.BaseApp {
	logger := defaultLogger()
	db := dbm.NewMemDB()
	codec := codec.NewLegacyAmino()
	registerTestCodec(codec)
	return baseapp.NewBaseApp(name, logger, db, testTxDecoder(codec), options...)
}

// simple one store baseapp
func setupBaseApp(t *testing.T, options ...func(*baseapp.BaseApp)) *baseapp.BaseApp {
	app := newBaseApp(t.Name(), options...)
	require.Equal(t, t.Name(), app.Name())

	app.MountStores(capKey1, capKey2)
	app.SetParamStore(&paramStore{db: dbm.NewMemDB()})

	// stores are mounted
	err := app.LoadLatestVersion()
	require.Nil(t, err)
	return app
}

func testTxHandler(options middleware.TxHandlerOptions) tx.Handler {
	return middleware.ComposeMiddlewares(
		middleware.NewRunMsgsTxHandler(options.MsgServiceRouter, middleware.NewLegacyRouter()),
		middleware.GasTxMiddleware,
		middleware.RecoveryTxMiddleware,
		middleware.NewIndexEventsTxMiddleware(options.IndexEvents),
	)
}

// simple one store baseapp with data and snapshots. Each tx is 1 MB in size (uncompressed).
func setupBaseAppWithSnapshots(t *testing.T, blocks uint, blockTxs int, options ...func(*baseapp.BaseApp)) (*baseapp.BaseApp, func()) {
	codec := codec.NewLegacyAmino()
	registerTestCodec(codec)
	routerOpt := func(bapp *baseapp.BaseApp) {
		legacyRouter := middleware.NewLegacyRouter()
		legacyRouter.AddRoute(sdk.NewRoute(routeMsgKeyValue, func(ctx sdk.Context, msg sdk.Msg) (*sdk.Result, error) {
			// TODO:
			// kv := msg.(*msgKeyValue)
			// bapp.cms.GetCommitKVStore(capKey2).Set(kv.Key, kv.Value)
			return &sdk.Result{}, nil
		}))
		txHandler := testTxHandler(middleware.TxHandlerOptions{
			LegacyRouter:      legacyRouter,
			MsgServiceRouter:  middleware.NewMsgServiceRouter(interfaceRegistry),
			LegacyAnteHandler: func(ctx sdk.Context, tx sdk.Tx, simulate bool) (sdk.Context, error) { return ctx, nil },
		})
		bapp.SetTxHandler(txHandler)
	}

	snapshotInterval := uint64(2)
	snapshotTimeout := 1 * time.Minute
	snapshotDir, err := ioutil.TempDir("", "baseapp")
	require.NoError(t, err)
	snapshotStore, err := snapshots.NewStore(dbm.NewMemDB(), snapshotDir)
	require.NoError(t, err)
	teardown := func() {
		os.RemoveAll(snapshotDir)
	}

	app := setupBaseApp(t, append(options,
		baseapp.SetSnapshotStore(snapshotStore),
		baseapp.SetSnapshotInterval(snapshotInterval),
		baseapp.SetPruning(sdk.PruningOptions{KeepEvery: 1}),
		routerOpt)...)

	app.InitChain(abci.RequestInitChain{})

	r := rand.New(rand.NewSource(3920758213583))
	keyCounter := 0
	for height := int64(1); height <= int64(blocks); height++ {
		app.BeginBlock(abci.RequestBeginBlock{Header: tmproto.Header{Height: height}})
		for txNum := 0; txNum < blockTxs; txNum++ {
			tx := txTest{Msgs: []sdk.Msg{}}
			for msgNum := 0; msgNum < 100; msgNum++ {
				key := []byte(fmt.Sprintf("%v", keyCounter))
				value := make([]byte, 10000)
				_, err := r.Read(value)
				require.NoError(t, err)
				tx.Msgs = append(tx.Msgs, msgKeyValue{Key: key, Value: value})
				keyCounter++
			}
			txBytes, err := codec.Marshal(tx)
			require.NoError(t, err)
			resp := app.DeliverTx(abci.RequestDeliverTx{Tx: txBytes})
			require.True(t, resp.IsOK(), "%v", resp.String())
		}
		app.EndBlock(abci.RequestEndBlock{Height: height})
		app.Commit()

		// Wait for snapshot to be taken, since it happens asynchronously.
		if uint64(height)%snapshotInterval == 0 {
			start := time.Now()
			for {
				if time.Since(start) > snapshotTimeout {
					t.Errorf("timed out waiting for snapshot after %v", snapshotTimeout)
				}
				snapshot, err := snapshotStore.Get(uint64(height), snapshottypes.CurrentFormat)
				require.NoError(t, err)
				if snapshot != nil {
					break
				}
				time.Sleep(100 * time.Millisecond)
			}
		}
	}

	return app, teardown
}

func TestListSnapshots(t *testing.T) {
	app, teardown := setupBaseAppWithSnapshots(t, 5, 4)
	defer teardown()

	resp := app.ListSnapshots(abci.RequestListSnapshots{})
	for _, s := range resp.Snapshots {
		assert.NotEmpty(t, s.Hash)
		assert.NotEmpty(t, s.Metadata)
		s.Hash = nil
		s.Metadata = nil
	}
	assert.Equal(t, abci.ResponseListSnapshots{Snapshots: []*abci.Snapshot{
		{Height: 4, Format: 1, Chunks: 2},
		{Height: 2, Format: 1, Chunks: 1},
	}}, resp)
}

func TestLoadSnapshotChunk(t *testing.T) {
	app, teardown := setupBaseAppWithSnapshots(t, 2, 5)
	defer teardown()

	testcases := map[string]struct {
		height      uint64
		format      uint32
		chunk       uint32
		expectEmpty bool
	}{
		"Existing snapshot": {2, 1, 1, false},
		"Missing height":    {100, 1, 1, true},
		"Missing format":    {2, 2, 1, true},
		"Missing chunk":     {2, 1, 9, true},
		"Zero height":       {0, 1, 1, true},
		"Zero format":       {2, 0, 1, true},
		"Zero chunk":        {2, 1, 0, false},
	}
	for name, tc := range testcases {
		tc := tc
		t.Run(name, func(t *testing.T) {
			resp := app.LoadSnapshotChunk(abci.RequestLoadSnapshotChunk{
				Height: tc.height,
				Format: tc.format,
				Chunk:  tc.chunk,
			})
			if tc.expectEmpty {
				assert.Equal(t, abci.ResponseLoadSnapshotChunk{}, resp)
				return
			}
			assert.NotEmpty(t, resp.Chunk)
		})
	}
}

func TestOfferSnapshot_Errors(t *testing.T) {
	// Set up app before test cases, since it's fairly expensive.
	app, teardown := setupBaseAppWithSnapshots(t, 0, 0)
	defer teardown()

	m := snapshottypes.Metadata{ChunkHashes: [][]byte{{1}, {2}, {3}}}
	metadata, err := m.Marshal()
	require.NoError(t, err)
	hash := []byte{1, 2, 3}

	testcases := map[string]struct {
		snapshot *abci.Snapshot
		result   abci.ResponseOfferSnapshot_Result
	}{
		"nil snapshot": {nil, abci.ResponseOfferSnapshot_REJECT},
		"invalid format": {&abci.Snapshot{
			Height: 1, Format: 9, Chunks: 3, Hash: hash, Metadata: metadata,
		}, abci.ResponseOfferSnapshot_REJECT_FORMAT},
		"incorrect chunk count": {&abci.Snapshot{
			Height: 1, Format: 1, Chunks: 2, Hash: hash, Metadata: metadata,
		}, abci.ResponseOfferSnapshot_REJECT},
		"no chunks": {&abci.Snapshot{
			Height: 1, Format: 1, Chunks: 0, Hash: hash, Metadata: metadata,
		}, abci.ResponseOfferSnapshot_REJECT},
		"invalid metadata serialization": {&abci.Snapshot{
			Height: 1, Format: 1, Chunks: 0, Hash: hash, Metadata: []byte{3, 1, 4},
		}, abci.ResponseOfferSnapshot_REJECT},
	}
	for name, tc := range testcases {
		tc := tc
		t.Run(name, func(t *testing.T) {
			resp := app.OfferSnapshot(abci.RequestOfferSnapshot{Snapshot: tc.snapshot})
			assert.Equal(t, tc.result, resp.Result)
		})
	}

	// Offering a snapshot after one has been accepted should error
	resp := app.OfferSnapshot(abci.RequestOfferSnapshot{Snapshot: &abci.Snapshot{
		Height:   1,
		Format:   snapshottypes.CurrentFormat,
		Chunks:   3,
		Hash:     []byte{1, 2, 3},
		Metadata: metadata,
	}})
	require.Equal(t, abci.ResponseOfferSnapshot{Result: abci.ResponseOfferSnapshot_ACCEPT}, resp)

	resp = app.OfferSnapshot(abci.RequestOfferSnapshot{Snapshot: &abci.Snapshot{
		Height:   2,
		Format:   snapshottypes.CurrentFormat,
		Chunks:   3,
		Hash:     []byte{1, 2, 3},
		Metadata: metadata,
	}})
	require.Equal(t, abci.ResponseOfferSnapshot{Result: abci.ResponseOfferSnapshot_ABORT}, resp)
}

func TestApplySnapshotChunk(t *testing.T) {
	source, teardown := setupBaseAppWithSnapshots(t, 4, 10)
	defer teardown()

	target, teardown := setupBaseAppWithSnapshots(t, 0, 0)
	defer teardown()

	// Fetch latest snapshot to restore
	respList := source.ListSnapshots(abci.RequestListSnapshots{})
	require.NotEmpty(t, respList.Snapshots)
	snapshot := respList.Snapshots[0]

	// Make sure the snapshot has at least 3 chunks
	require.GreaterOrEqual(t, snapshot.Chunks, uint32(3), "Not enough snapshot chunks")

	// Begin a snapshot restoration in the target
	respOffer := target.OfferSnapshot(abci.RequestOfferSnapshot{Snapshot: snapshot})
	require.Equal(t, abci.ResponseOfferSnapshot{Result: abci.ResponseOfferSnapshot_ACCEPT}, respOffer)

	// We should be able to pass an invalid chunk and get a verify failure, before reapplying it.
	respApply := target.ApplySnapshotChunk(abci.RequestApplySnapshotChunk{
		Index:  0,
		Chunk:  []byte{9},
		Sender: "sender",
	})
	require.Equal(t, abci.ResponseApplySnapshotChunk{
		Result:        abci.ResponseApplySnapshotChunk_RETRY,
		RefetchChunks: []uint32{0},
		RejectSenders: []string{"sender"},
	}, respApply)

	// Fetch each chunk from the source and apply it to the target
	for index := uint32(0); index < snapshot.Chunks; index++ {
		respChunk := source.LoadSnapshotChunk(abci.RequestLoadSnapshotChunk{
			Height: snapshot.Height,
			Format: snapshot.Format,
			Chunk:  index,
		})
		require.NotNil(t, respChunk.Chunk)
		respApply := target.ApplySnapshotChunk(abci.RequestApplySnapshotChunk{
			Index: index,
			Chunk: respChunk.Chunk,
		})
		require.Equal(t, abci.ResponseApplySnapshotChunk{
			Result: abci.ResponseApplySnapshotChunk_ACCEPT,
		}, respApply)
	}

	// The target should now have the same hash as the source
	assert.Equal(t, source.LastCommitID(), target.LastCommitID())
}

func anteHandlerTxTest(t *testing.T, capKey sdk.StoreKey, storeKey []byte) sdk.AnteHandler {
	return func(ctx sdk.Context, tx sdk.Tx, simulate bool) (sdk.Context, error) {
		store := ctx.KVStore(capKey)
		txTest := tx.(txTest)

		if txTest.FailOnAnte {
			return ctx, sdkerrors.Wrap(sdkerrors.ErrUnauthorized, "ante handler failure")
		}

		_, err := incrementingCounter(t, store, storeKey, txTest.Counter)
		if err != nil {
			return ctx, err
		}

		ctx.EventManager().EmitEvents(
			counterEvent("ante_handler", txTest.Counter),
		)

		return ctx, nil
	}
}

func counterEvent(evType string, msgCount int64) sdk.Events {
	return sdk.Events{
		sdk.NewEvent(
			evType,
			sdk.NewAttribute("update_counter", fmt.Sprintf("%d", msgCount)),
		),
	}
}

func handlerMsgCounter(t *testing.T, capKey sdk.StoreKey, deliverKey []byte) sdk.Handler {
	return func(ctx sdk.Context, msg sdk.Msg) (*sdk.Result, error) {
		ctx = ctx.WithEventManager(sdk.NewEventManager())
		store := ctx.KVStore(capKey)
		var msgCount int64

		switch m := msg.(type) {
		case *msgCounter:
			if m.FailOnHandler {
				return nil, sdkerrors.Wrap(sdkerrors.ErrInvalidRequest, "message handler failure")
			}

			msgCount = m.Counter
		case *msgCounter2:
			msgCount = m.Counter
		}

		ctx.EventManager().EmitEvents(
			counterEvent(sdk.EventTypeMessage, msgCount),
		)

		res, err := incrementingCounter(t, store, deliverKey, msgCount)
		if err != nil {
			return nil, err
		}

		res.Events = ctx.EventManager().Events().ToABCIEvents()
		return res, nil
	}
}

func setIntOnStore(store sdk.KVStore, key []byte, i int64) {
	bz := make([]byte, 8)
	n := binary.PutVarint(bz, i)
	store.Set(key, bz[:n])
}

func getIntFromStore(store sdk.KVStore, key []byte) int64 {
	bz := store.Get(key)
	if len(bz) == 0 {
		return 0
	}
	i, err := binary.ReadVarint(bytes.NewBuffer(bz))
	if err != nil {
		panic(err)
	}
	return i
}

// check counter matches what's in store.
// increment and store
func incrementingCounter(t *testing.T, store sdk.KVStore, counterKey []byte, counter int64) (*sdk.Result, error) {
	storedCounter := getIntFromStore(store, counterKey)
	require.Equal(t, storedCounter, counter)
	setIntOnStore(store, counterKey, counter+1)
	return &sdk.Result{}, nil
}

func newTxCounter(counter int64, msgCounters ...int64) txTest {
	msgs := make([]sdk.Msg, 0, len(msgCounters))
	for _, c := range msgCounters {
		msgs = append(msgs, msgCounter{c, false})
	}

	return txTest{msgs, counter, false, math.MaxUint64}
}

//---------------------------------------------------------------------
// Tx processing - CheckTx, DeliverTx, SimulateTx.
// These tests use the serialized tx as input, while most others will use the
// Check(), Deliver(), Simulate() methods directly.
// Ensure that Check/Deliver/Simulate work as expected with the store.

// Test that successive CheckTx can see each others' effects
// on the store within a block, and that the CheckTx state
// gets reset to the latest committed state during Commit
func TestCheckTx(t *testing.T) {
	// This ante handler reads the key and checks that the value matches the current counter.
	// This ensures changes to the kvstore persist across successive CheckTx.
	counterKey := []byte("counter-key")

	txHandlerOpt := func(bapp *baseapp.BaseApp) {
		legacyRouter := middleware.NewLegacyRouter()
		// TODO: can remove this once CheckTx doesnt process msgs.
		legacyRouter.AddRoute(sdk.NewRoute(routeMsgCounter, func(ctx sdk.Context, msg sdk.Msg) (*sdk.Result, error) {
			return &sdk.Result{}, nil
		}))
		txHandler := testTxHandler(middleware.TxHandlerOptions{
			LegacyRouter:     legacyRouter,
			MsgServiceRouter: middleware.NewMsgServiceRouter(interfaceRegistry),
		})
		bapp.SetTxHandler(txHandler)
	}

	app := setupBaseApp(t, txHandlerOpt)

	nTxs := int64(5)
	app.InitChain(abci.RequestInitChain{})

	// Create same codec used in txDecoder
	codec := codec.NewLegacyAmino()
	registerTestCodec(codec)

	for i := int64(0); i < nTxs; i++ {
		tx := newTxCounter(i, 0) // no messages
		txBytes, err := codec.Marshal(tx)
		require.NoError(t, err)
		r := app.CheckTx(abci.RequestCheckTx{Tx: txBytes})
		require.Empty(t, r.GetEvents())
		require.True(t, r.IsOK(), fmt.Sprintf("%v", r))
	}

	checkStateStore := app.checkState.ctx.KVStore(capKey1)
	storedCounter := getIntFromStore(checkStateStore, counterKey)

	// Ensure AnteHandler ran
	require.Equal(t, nTxs, storedCounter)

	// If a block is committed, CheckTx state should be reset.
	header := tmproto.Header{Height: 1}
	app.BeginBlock(abci.RequestBeginBlock{Header: header, Hash: []byte("hash")})

	require.NotNil(t, app.checkState.ctx.BlockGasMeter(), "block gas meter should have been set to checkState")
	require.NotEmpty(t, app.checkState.ctx.HeaderHash())

	app.EndBlock(abci.RequestEndBlock{})
	app.Commit()

	checkStateStore = app.checkState.ctx.KVStore(capKey1)
	storedBytes := checkStateStore.Get(counterKey)
	require.Nil(t, storedBytes)
}

// Test that successive DeliverTx can see each others' effects
// on the store, both within and across blocks.
func TestDeliverTx(t *testing.T) {
	// test increments in the ante
	anteKey := []byte("ante-key")
	// test increments in the handler
	deliverKey := []byte("deliver-key")
	txHandlerOpt := func(bapp *baseapp.BaseApp) {
		legacyRouter := middleware.NewLegacyRouter()
		r := sdk.NewRoute(routeMsgCounter, handlerMsgCounter(t, capKey1, deliverKey))
		legacyRouter.AddRoute(r)
		txHandler := testTxHandler(middleware.TxHandlerOptions{
			LegacyRouter:      legacyRouter,
			MsgServiceRouter:  middleware.NewMsgServiceRouter(interfaceRegistry),
			LegacyAnteHandler: anteHandlerTxTest(t, capKey1, anteKey),
		})
		bapp.SetTxHandler(txHandler)
	}
	app := setupBaseApp(t, txHandlerOpt)
	app.InitChain(abci.RequestInitChain{})

	// Create same codec used in txDecoder
	codec := codec.NewLegacyAmino()
	registerTestCodec(codec)

	nBlocks := 3
	txPerHeight := 5

	for blockN := 0; blockN < nBlocks; blockN++ {
		header := tmproto.Header{Height: int64(blockN) + 1}
		app.BeginBlock(abci.RequestBeginBlock{Header: header})

		for i := 0; i < txPerHeight; i++ {
			counter := int64(blockN*txPerHeight + i)
			tx := newTxCounter(counter, counter)

			txBytes, err := codec.Marshal(tx)
			require.NoError(t, err)

			res := app.DeliverTx(abci.RequestDeliverTx{Tx: txBytes})
			require.True(t, res.IsOK(), fmt.Sprintf("%v", res))
			events := res.GetEvents()
			require.Len(t, events, 3, "should contain ante handler, message type and counter events respectively")
			require.Equal(t, sdk.MarkEventsToIndex(counterEvent("ante_handler", counter).ToABCIEvents(), map[string]struct{}{})[0], events[0], "ante handler event")
			require.Equal(t, sdk.MarkEventsToIndex(counterEvent(sdk.EventTypeMessage, counter).ToABCIEvents(), map[string]struct{}{})[0], events[2], "msg handler update counter event")
		}

		app.EndBlock(abci.RequestEndBlock{})
		app.Commit()
	}
}

// Number of messages doesn't matter to CheckTx.
func TestMultiMsgCheckTx(t *testing.T) {
	// TODO: ensure we get the same results
	// with one message or many
}

// One call to DeliverTx should process all the messages, in order.
func TestMultiMsgDeliverTx(t *testing.T) {
	// increment the tx counter
	anteKey := []byte("ante-key")
	// increment the msg counter
	deliverKey := []byte("deliver-key")
	deliverKey2 := []byte("deliver-key2")
	txHandlerOpt := func(bapp *baseapp.BaseApp) {
		legacyRouter := middleware.NewLegacyRouter()
		r1 := sdk.NewRoute(routeMsgCounter, handlerMsgCounter(t, capKey1, deliverKey))
		r2 := sdk.NewRoute(routeMsgCounter2, handlerMsgCounter(t, capKey1, deliverKey2))
		legacyRouter.AddRoute(r1)
		legacyRouter.AddRoute(r2)
		txHandler := testTxHandler(middleware.TxHandlerOptions{
			LegacyRouter:      legacyRouter,
			MsgServiceRouter:  middleware.NewMsgServiceRouter(interfaceRegistry),
			LegacyAnteHandler: anteHandlerTxTest(t, capKey1, anteKey),
		})
		bapp.SetTxHandler(txHandler)
	}
	app := setupBaseApp(t, txHandlerOpt)

	// Create same codec used in txDecoder
	codec := codec.NewLegacyAmino()
	registerTestCodec(codec)

	// run a multi-msg tx
	// with all msgs the same route

	header := tmproto.Header{Height: 1}
	app.BeginBlock(abci.RequestBeginBlock{Header: header})
	tx := newTxCounter(0, 0, 1, 2)
	txBytes, err := codec.Marshal(tx)
	require.NoError(t, err)
	res := app.DeliverTx(abci.RequestDeliverTx{Tx: txBytes})
	require.True(t, res.IsOK(), fmt.Sprintf("%v", res))

	store := app.deliverState.ctx.KVStore(capKey1)

	// tx counter only incremented once
	txCounter := getIntFromStore(store, anteKey)
	require.Equal(t, int64(1), txCounter)

	// msg counter incremented three times
	msgCounter := getIntFromStore(store, deliverKey)
	require.Equal(t, int64(3), msgCounter)

	// replace the second message with a msgCounter2

	tx = newTxCounter(1, 3)
	tx.Msgs = append(tx.Msgs, msgCounter2{0})
	tx.Msgs = append(tx.Msgs, msgCounter2{1})
	txBytes, err = codec.Marshal(tx)
	require.NoError(t, err)
	res = app.DeliverTx(abci.RequestDeliverTx{Tx: txBytes})
	require.True(t, res.IsOK(), fmt.Sprintf("%v", res))

	store = app.deliverState.ctx.KVStore(capKey1)

	// tx counter only incremented once
	txCounter = getIntFromStore(store, anteKey)
	require.Equal(t, int64(2), txCounter)

	// original counter increments by one
	// new counter increments by two
	msgCounter = getIntFromStore(store, deliverKey)
	require.Equal(t, int64(4), msgCounter)
	msgCounter2 := getIntFromStore(store, deliverKey2)
	require.Equal(t, int64(2), msgCounter2)
}

// Interleave calls to Check and Deliver and ensure
// that there is no cross-talk. Check sees results of the previous Check calls
// and Deliver sees that of the previous Deliver calls, but they don't see eachother.
func TestConcurrentCheckDeliver(t *testing.T) {
	// TODO
}

// Simulate a transaction that uses gas to compute the gas.
// Simulate() and Query("/app/simulate", txBytes) should give
// the same results.
func TestSimulateTx(t *testing.T) {
	gasConsumed := uint64(5)

	txHandlerOpt := func(bapp *baseapp.BaseApp) {
		legacyRouter := middleware.NewLegacyRouter()
		r := sdk.NewRoute(routeMsgCounter, func(ctx sdk.Context, msg sdk.Msg) (*sdk.Result, error) {
			ctx.GasMeter().ConsumeGas(gasConsumed, "test")
			return &sdk.Result{}, nil
		})
		legacyRouter.AddRoute(r)
		txHandler := testTxHandler(middleware.TxHandlerOptions{
			LegacyRouter:      legacyRouter,
			MsgServiceRouter:  middleware.NewMsgServiceRouter(interfaceRegistry),
			LegacyAnteHandler: func(ctx sdk.Context, tx sdk.Tx, simulate bool) (sdk.Context, error) { return ctx, nil },
		})
		bapp.SetTxHandler(txHandler)
	}
	app := setupBaseApp(t, txHandlerOpt)

	app.InitChain(abci.RequestInitChain{})

	// Create same codec used in txDecoder
	cdc := codec.NewLegacyAmino()
	registerTestCodec(cdc)

	nBlocks := 3
	for blockN := 0; blockN < nBlocks; blockN++ {
		count := int64(blockN + 1)
		header := tmproto.Header{Height: count}
		app.BeginBlock(abci.RequestBeginBlock{Header: header})

		tx := newTxCounter(count, count)
		tx.GasLimit = gasConsumed
		txBytes, err := cdc.Marshal(tx)
		require.Nil(t, err)

		// simulate a message, check gas reported
		gInfo, result, err := app.Simulate(txBytes)
		require.NoError(t, err)
		require.NotNil(t, result)
		require.Equal(t, gasConsumed, gInfo.GasUsed)

		// simulate again, same result
		gInfo, result, err = app.Simulate(txBytes)
		require.NoError(t, err)
		require.NotNil(t, result)
		require.Equal(t, gasConsumed, gInfo.GasUsed)

		// simulate by calling Query with encoded tx
		query := abci.RequestQuery{
			Path: "/app/simulate",
			Data: txBytes,
		}
		queryResult := app.Query(query)
		require.True(t, queryResult.IsOK(), queryResult.Log)

		var simRes sdk.SimulationResponse
		require.NoError(t, jsonpb.Unmarshal(strings.NewReader(string(queryResult.Value)), &simRes))

		require.Equal(t, gInfo, simRes.GasInfo)
		require.Equal(t, result.Log, simRes.Result.Log)
		require.Equal(t, result.Events, simRes.Result.Events)
		require.True(t, bytes.Equal(result.Data, simRes.Result.Data))

		app.EndBlock(abci.RequestEndBlock{})
		app.Commit()
	}
}

func TestRunInvalidTransaction(t *testing.T) {
	txHandlerOpt := func(bapp *baseapp.BaseApp) {
		legacyRouter := middleware.NewLegacyRouter()
		r := sdk.NewRoute(routeMsgCounter, func(ctx sdk.Context, msg sdk.Msg) (*sdk.Result, error) {
			return &sdk.Result{}, nil
		})
		legacyRouter.AddRoute(r)
		txHandler := testTxHandler(middleware.TxHandlerOptions{
			LegacyRouter:     legacyRouter,
			MsgServiceRouter: middleware.NewMsgServiceRouter(interfaceRegistry),
			LegacyAnteHandler: func(ctx sdk.Context, tx sdk.Tx, simulate bool) (newCtx sdk.Context, err error) {
				return
			},
		})
		bapp.SetTxHandler(txHandler)
	}
	app := setupBaseApp(t, txHandlerOpt)

	header := tmproto.Header{Height: 1}
	app.BeginBlock(abci.RequestBeginBlock{Header: header})

	// transaction with no messages
	{
		emptyTx := &txTest{}
		_, result, err := app.SimDeliver(aminoTxEncoder(), emptyTx)
		require.Nil(t, result)

		space, code, _ := sdkerrors.ABCIInfo(err, false)
		require.EqualValues(t, sdkerrors.ErrInvalidRequest.Codespace(), space, err)
		require.EqualValues(t, sdkerrors.ErrInvalidRequest.ABCICode(), code, err)
	}

	// transaction where ValidateBasic fails
	{
		testCases := []struct {
			tx   txTest
			fail bool
		}{
			{newTxCounter(0, 0), false},
			{newTxCounter(-1, 0), false},
			{newTxCounter(100, 100), false},
			{newTxCounter(100, 5, 4, 3, 2, 1), false},

			{newTxCounter(0, -1), true},
			{newTxCounter(0, 1, -2), true},
			{newTxCounter(0, 1, 2, -10, 5), true},
		}

		for _, testCase := range testCases {
			tx := testCase.tx
			_, result, err := app.SimDeliver(aminoTxEncoder(), tx)

			if testCase.fail {
				require.Error(t, err)

				space, code, _ := sdkerrors.ABCIInfo(err, false)
				require.EqualValues(t, sdkerrors.ErrInvalidSequence.Codespace(), space, err)
				require.EqualValues(t, sdkerrors.ErrInvalidSequence.ABCICode(), code, err)
			} else {
				require.NotNil(t, result)
			}
		}
	}

	// transaction with no known route
	{
		unknownRouteTx := txTest{[]sdk.Msg{msgNoRoute{}}, 0, false, math.MaxUint64}
		_, result, err := app.SimDeliver(aminoTxEncoder(), unknownRouteTx)
		require.Error(t, err)
		require.Nil(t, result)

		space, code, _ := sdkerrors.ABCIInfo(err, false)
		require.EqualValues(t, sdkerrors.ErrUnknownRequest.Codespace(), space, err)
		require.EqualValues(t, sdkerrors.ErrUnknownRequest.ABCICode(), code, err)

		unknownRouteTx = txTest{[]sdk.Msg{msgCounter{}, msgNoRoute{}}, 0, false, math.MaxUint64}
		_, result, err = app.SimDeliver(aminoTxEncoder(), unknownRouteTx)
		require.Error(t, err)
		require.Nil(t, result)

		space, code, _ = sdkerrors.ABCIInfo(err, false)
		require.EqualValues(t, sdkerrors.ErrUnknownRequest.Codespace(), space, err)
		require.EqualValues(t, sdkerrors.ErrUnknownRequest.ABCICode(), code, err)
	}

	// Transaction with an unregistered message
	{
		tx := newTxCounter(0, 0)
		tx.Msgs = append(tx.Msgs, msgNoDecode{})

		// new codec so we can encode the tx, but we shouldn't be able to decode
		newCdc := codec.NewLegacyAmino()
		registerTestCodec(newCdc)
		newCdc.RegisterConcrete(&msgNoDecode{}, "cosmos-sdk/baseapp/msgNoDecode", nil)

		txBytes, err := newCdc.Marshal(tx)
		require.NoError(t, err)

		res := app.DeliverTx(abci.RequestDeliverTx{Tx: txBytes})
		require.EqualValues(t, sdkerrors.ErrTxDecode.ABCICode(), res.Code)
		require.EqualValues(t, sdkerrors.ErrTxDecode.Codespace(), res.Codespace)
	}
}

// Test that transactions exceeding gas limits fail
func TestTxGasLimits(t *testing.T) {
	gasGranted := uint64(10)

	ante := func(ctx sdk.Context, tx sdk.Tx, simulate bool) (sdk.Context, error) {
		count := tx.(txTest).Counter
		ctx.GasMeter().ConsumeGas(uint64(count), "counter-ante")
		return ctx, nil
	}

	txHandlerOpt := func(bapp *baseapp.BaseApp) {
		legacyRouter := middleware.NewLegacyRouter()
		r := sdk.NewRoute(routeMsgCounter, func(ctx sdk.Context, msg sdk.Msg) (*sdk.Result, error) {
			count := msg.(msgCounter).Counter
			ctx.GasMeter().ConsumeGas(uint64(count), "counter-handler")
			return &sdk.Result{}, nil
		})
		legacyRouter.AddRoute(r)
		txHandler := testTxHandler(middleware.TxHandlerOptions{
			LegacyRouter:      legacyRouter,
			MsgServiceRouter:  middleware.NewMsgServiceRouter(interfaceRegistry),
			LegacyAnteHandler: ante,
		})

		bapp.SetTxHandler(txHandler)
	}
	app := setupBaseApp(t, txHandlerOpt)

	header := tmproto.Header{Height: 1}
	app.BeginBlock(abci.RequestBeginBlock{Header: header})

	testCases := []struct {
		tx      txTest
		gasUsed uint64
		fail    bool
	}{
		{newTxCounter(0, 0), 0, false},
		{newTxCounter(1, 1), 2, false},
		{newTxCounter(9, 1), 10, false},
		{newTxCounter(1, 9), 10, false},
		{newTxCounter(10, 0), 10, false},
		{newTxCounter(0, 10), 10, false},
		{newTxCounter(0, 8, 2), 10, false},
		{newTxCounter(0, 5, 1, 1, 1, 1, 1), 10, false},
		{newTxCounter(0, 5, 1, 1, 1, 1), 9, false},

		{newTxCounter(9, 2), 11, true},
		{newTxCounter(2, 9), 11, true},
		{newTxCounter(9, 1, 1), 11, true},
		{newTxCounter(1, 8, 1, 1), 11, true},
		{newTxCounter(11, 0), 11, true},
		{newTxCounter(0, 11), 11, true},
		{newTxCounter(0, 5, 11), 16, true},
	}

	for i, tc := range testCases {
		tx := tc.tx
		tx.GasLimit = gasGranted
		gInfo, result, err := app.SimDeliver(aminoTxEncoder(), tx)

		// check gas used and wanted
		require.Equal(t, tc.gasUsed, gInfo.GasUsed, fmt.Sprintf("tc #%d; gas: %v, result: %v, err: %s", i, gInfo, result, err))

		// check for out of gas
		if !tc.fail {
			require.NotNil(t, result, fmt.Sprintf("%d: %v, %v", i, tc, err))
		} else {
			require.Error(t, err)
			require.Nil(t, result)

			space, code, _ := sdkerrors.ABCIInfo(err, false)
			require.EqualValues(t, sdkerrors.ErrOutOfGas.Codespace(), space, err)
			require.EqualValues(t, sdkerrors.ErrOutOfGas.ABCICode(), code, err)
		}
	}
}

// Test that transactions exceeding gas limits fail
func TestMaxBlockGasLimits(t *testing.T) {
	gasGranted := uint64(10)
	ante := func(ctx sdk.Context, tx sdk.Tx, simulate bool) (sdk.Context, error) {
		count := tx.(txTest).Counter
		ctx.GasMeter().ConsumeGas(uint64(count), "counter-ante")

		return ctx, nil
	}

	txHandlerOpt := func(bapp *baseapp.BaseApp) {
		legacyRouter := middleware.NewLegacyRouter()
		r := sdk.NewRoute(routeMsgCounter, func(ctx sdk.Context, msg sdk.Msg) (*sdk.Result, error) {
			count := msg.(msgCounter).Counter
			ctx.GasMeter().ConsumeGas(uint64(count), "counter-handler")
			return &sdk.Result{}, nil
		})
		legacyRouter.AddRoute(r)
		txHandler := testTxHandler(middleware.TxHandlerOptions{
			LegacyRouter:      legacyRouter,
			MsgServiceRouter:  middleware.NewMsgServiceRouter(interfaceRegistry),
			LegacyAnteHandler: ante,
		})
		bapp.SetTxHandler(txHandler)
	}
	app := setupBaseApp(t, txHandlerOpt)

	app.InitChain(abci.RequestInitChain{
		ConsensusParams: &abci.ConsensusParams{
			Block: &abci.BlockParams{
				MaxGas: 100,
			},
		},
	})

	testCases := []struct {
		tx                txTest
		numDelivers       int
		gasUsedPerDeliver uint64
		fail              bool
		failAfterDeliver  int
	}{
		{newTxCounter(0, 0), 0, 0, false, 0},
		{newTxCounter(9, 1), 2, 10, false, 0},
		{newTxCounter(10, 0), 3, 10, false, 0},
		{newTxCounter(10, 0), 10, 10, false, 0},
		{newTxCounter(2, 7), 11, 9, false, 0},
		{newTxCounter(10, 0), 10, 10, false, 0}, // hit the limit but pass

		{newTxCounter(10, 0), 11, 10, true, 10},
		{newTxCounter(10, 0), 15, 10, true, 10},
		{newTxCounter(9, 0), 12, 9, true, 11}, // fly past the limit
	}

	for i, tc := range testCases {
		tx := tc.tx
		tx.GasLimit = gasGranted

		// reset the block gas
		header := tmproto.Header{Height: app.LastBlockHeight() + 1}
		app.BeginBlock(abci.RequestBeginBlock{Header: header})

		// execute the transaction multiple times
		for j := 0; j < tc.numDelivers; j++ {
			_, result, err := app.SimDeliver(aminoTxEncoder(), tx)

			ctx := app.getState(runTxModeDeliver).ctx

			// check for failed transactions
			if tc.fail && (j+1) > tc.failAfterDeliver {
				require.Error(t, err, fmt.Sprintf("tc #%d; result: %v, err: %s", i, result, err))
				require.Nil(t, result, fmt.Sprintf("tc #%d; result: %v, err: %s", i, result, err))

				space, code, _ := sdkerrors.ABCIInfo(err, false)
				require.EqualValues(t, sdkerrors.ErrOutOfGas.Codespace(), space, err)
				require.EqualValues(t, sdkerrors.ErrOutOfGas.ABCICode(), code, err)
				require.True(t, ctx.BlockGasMeter().IsOutOfGas())
			} else {
				// check gas used and wanted
				blockGasUsed := ctx.BlockGasMeter().GasConsumed()
				expBlockGasUsed := tc.gasUsedPerDeliver * uint64(j+1)
				require.Equal(
					t, expBlockGasUsed, blockGasUsed,
					fmt.Sprintf("%d,%d: %v, %v, %v, %v", i, j, tc, expBlockGasUsed, blockGasUsed, result),
				)

				require.NotNil(t, result, fmt.Sprintf("tc #%d; currDeliver: %d, result: %v, err: %s", i, j, result, err))
				require.False(t, ctx.BlockGasMeter().IsPastLimit())
			}
		}
	}
}

func TestBaseAppAnteHandler(t *testing.T) {
	anteKey := []byte("ante-key")
	deliverKey := []byte("deliver-key")
	cdc := codec.NewLegacyAmino()

	txHandlerOpt := func(bapp *baseapp.BaseApp) {
		legacyRouter := middleware.NewLegacyRouter()
		r := sdk.NewRoute(routeMsgCounter, handlerMsgCounter(t, capKey1, deliverKey))
		legacyRouter.AddRoute(r)
		txHandler := testTxHandler(middleware.TxHandlerOptions{
			LegacyRouter:      legacyRouter,
			MsgServiceRouter:  middleware.NewMsgServiceRouter(interfaceRegistry),
			LegacyAnteHandler: anteHandlerTxTest(t, capKey1, anteKey),
		})
		bapp.SetTxHandler(txHandler)
	}
	app := setupBaseApp(t, txHandlerOpt)

	app.InitChain(abci.RequestInitChain{})
	registerTestCodec(cdc)

	header := tmproto.Header{Height: app.LastBlockHeight() + 1}
	app.BeginBlock(abci.RequestBeginBlock{Header: header})

	// execute a tx that will fail ante handler execution
	//
	// NOTE: State should not be mutated here. This will be implicitly checked by
	// the next txs ante handler execution (anteHandlerTxTest).
	tx := newTxCounter(0, 0)
	tx.setFailOnAnte(true)
	txBytes, err := cdc.Marshal(tx)
	require.NoError(t, err)
	res := app.DeliverTx(abci.RequestDeliverTx{Tx: txBytes})
	require.Empty(t, res.Events)
	require.False(t, res.IsOK(), fmt.Sprintf("%v", res))

	ctx := app.getState(runTxModeDeliver).ctx
	store := ctx.KVStore(capKey1)
	require.Equal(t, int64(0), getIntFromStore(store, anteKey))

	// execute at tx that will pass the ante handler (the checkTx state should
	// mutate) but will fail the message handler
	tx = newTxCounter(0, 0)
	tx.setFailOnHandler(true)

	txBytes, err = cdc.Marshal(tx)
	require.NoError(t, err)

	res = app.DeliverTx(abci.RequestDeliverTx{Tx: txBytes})
	require.Empty(t, res.Events)
	require.False(t, res.IsOK(), fmt.Sprintf("%v", res))

	ctx = app.getState(runTxModeDeliver).ctx
	store = ctx.KVStore(capKey1)
	require.Equal(t, int64(1), getIntFromStore(store, anteKey))
	require.Equal(t, int64(0), getIntFromStore(store, deliverKey))

	// execute a successful ante handler and message execution where state is
	// implicitly checked by previous tx executions
	tx = newTxCounter(1, 0)

	txBytes, err = cdc.Marshal(tx)
	require.NoError(t, err)

	res = app.DeliverTx(abci.RequestDeliverTx{Tx: txBytes})
	require.NotEmpty(t, res.Events)
	require.True(t, res.IsOK(), fmt.Sprintf("%v", res))

	ctx = app.getState(runTxModeDeliver).ctx
	store = ctx.KVStore(capKey1)
	require.Equal(t, int64(2), getIntFromStore(store, anteKey))
	require.Equal(t, int64(1), getIntFromStore(store, deliverKey))

	// commit
	app.EndBlock(abci.RequestEndBlock{})
	app.Commit()
}

func TestGasConsumptionBadTx(t *testing.T) {
	gasWanted := uint64(5)
	ante := func(ctx sdk.Context, tx sdk.Tx, simulate bool) (sdk.Context, error) {
		txTest := tx.(txTest)
		ctx.GasMeter().ConsumeGas(uint64(txTest.Counter), "counter-ante")
		if txTest.FailOnAnte {
			return ctx, sdkerrors.Wrap(sdkerrors.ErrUnauthorized, "ante handler failure")
		}

		return ctx, nil
	}

	cdc := codec.NewLegacyAmino()
	registerTestCodec(cdc)

	txHandlerOpt := func(bapp *baseapp.BaseApp) {
		legacyRouter := middleware.NewLegacyRouter()
		r := sdk.NewRoute(routeMsgCounter, func(ctx sdk.Context, msg sdk.Msg) (*sdk.Result, error) {
			count := msg.(msgCounter).Counter
			ctx.GasMeter().ConsumeGas(uint64(count), "counter-handler")
			return &sdk.Result{}, nil
		})
		legacyRouter.AddRoute(r)
		txHandler := testTxHandler(middleware.TxHandlerOptions{
			LegacyRouter:      legacyRouter,
			MsgServiceRouter:  middleware.NewMsgServiceRouter(interfaceRegistry),
			LegacyAnteHandler: ante,
		})
		bapp.SetTxHandler(txHandler)
	}
	app := setupBaseApp(t, txHandlerOpt)

	app.InitChain(abci.RequestInitChain{
		ConsensusParams: &abci.ConsensusParams{
			Block: &abci.BlockParams{
				MaxGas: 9,
			},
		},
	})

	app.InitChain(abci.RequestInitChain{})

	header := tmproto.Header{Height: app.LastBlockHeight() + 1}
	app.BeginBlock(abci.RequestBeginBlock{Header: header})

	tx := newTxCounter(5, 0)
	tx.GasLimit = gasWanted
	tx.setFailOnAnte(true)
	txBytes, err := cdc.Marshal(tx)
	require.NoError(t, err)

	res := app.DeliverTx(abci.RequestDeliverTx{Tx: txBytes})
	require.False(t, res.IsOK(), fmt.Sprintf("%v", res))

	// require next tx to fail due to black gas limit
	tx = newTxCounter(5, 0)
	txBytes, err = cdc.Marshal(tx)
	require.NoError(t, err)

	res = app.DeliverTx(abci.RequestDeliverTx{Tx: txBytes})
	require.False(t, res.IsOK(), fmt.Sprintf("%v", res))
}

// Test that we can only query from the latest committed state.
func TestQuery(t *testing.T) {
	key, value := []byte("hello"), []byte("goodbye")

	txHandlerOpt := func(bapp *baseapp.BaseApp) {
		legacyRouter := middleware.NewLegacyRouter()
		r := sdk.NewRoute(routeMsgCounter, func(ctx sdk.Context, msg sdk.Msg) (*sdk.Result, error) {
			store := ctx.KVStore(capKey1)
			store.Set(key, value)
			return &sdk.Result{}, nil
		})
		legacyRouter.AddRoute(r)
		txHandler := testTxHandler(middleware.TxHandlerOptions{
			LegacyRouter:     legacyRouter,
			MsgServiceRouter: middleware.NewMsgServiceRouter(interfaceRegistry),
			LegacyAnteHandler: func(ctx sdk.Context, tx sdk.Tx, simulate bool) (newCtx sdk.Context, err error) {
				store := ctx.KVStore(capKey1)
				store.Set(key, value)
				return
			},
		})
		bapp.SetTxHandler(txHandler)
	}
	app := setupBaseApp(t, txHandlerOpt)

	app.InitChain(abci.RequestInitChain{})

	// NOTE: "/store/key1" tells us KVStore
	// and the final "/key" says to use the data as the
	// key in the given KVStore ...
	query := abci.RequestQuery{
		Path: "/store/key1/key",
		Data: key,
	}
	tx := newTxCounter(0, 0)

	// query is empty before we do anything
	res := app.Query(query)
	require.Equal(t, 0, len(res.Value))

	// query is still empty after a CheckTx
	_, resTx, err := app.SimCheck(aminoTxEncoder(), tx)
	require.NoError(t, err)
	require.NotNil(t, resTx)
	res = app.Query(query)
	require.Equal(t, 0, len(res.Value))

	// query is still empty after a DeliverTx before we commit
	header := tmproto.Header{Height: app.LastBlockHeight() + 1}
	app.BeginBlock(abci.RequestBeginBlock{Header: header})

	_, resTx, err = app.SimDeliver(aminoTxEncoder(), tx)
	require.NoError(t, err)
	require.NotNil(t, resTx)
	res = app.Query(query)
	require.Equal(t, 0, len(res.Value))

	// query returns correct value after Commit
	app.Commit()
	res = app.Query(query)
	require.Equal(t, value, res.Value)
}