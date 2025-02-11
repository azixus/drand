package core

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"time"

	clock "github.com/jonboulle/clockwork"
	"go.opentelemetry.io/otel/attribute"

	"github.com/drand/drand/v2/common"
	public "github.com/drand/drand/v2/common/chain"
	"github.com/drand/drand/v2/common/key"
	"github.com/drand/drand/v2/common/tracer"
	"github.com/drand/drand/v2/crypto"
	"github.com/drand/drand/v2/internal/chain"
	"github.com/drand/drand/v2/internal/chain/beacon"
	"github.com/drand/drand/v2/internal/fs"
	"github.com/drand/drand/v2/internal/net"
	"github.com/drand/drand/v2/protobuf/drand"
)

// PublicKey is a functionality of Control Service defined in protobuf/control
// that requests the long term public key of the drand node running locally
func (bp *BeaconProcess) PublicKey(ctx context.Context, _ *drand.PublicKeyRequest) (*drand.PublicKeyResponse, error) {
	_, span := tracer.NewSpan(ctx, "bp.PublicKey")
	defer span.End()

	bp.state.RLock()
	defer bp.state.RUnlock()

	keyPair, err := bp.store.LoadKeyPair()
	if err != nil {
		return nil, err
	}

	protoKey, err := keyPair.Public.Key.MarshalBinary()
	if err != nil {
		return nil, err
	}

	return &drand.PublicKeyResponse{
		PubKey:     protoKey,
		Addr:       keyPair.Public.Addr,
		Signature:  keyPair.Public.Signature,
		Metadata:   bp.newMetadata(),
		SchemeName: keyPair.Public.Scheme.Name,
	}, nil
}

var ErrNoGroupSetup = errors.New("drand: no dkg group setup yet")

// GroupFile replies with the distributed key in the response
func (bp *BeaconProcess) GroupFile(ctx context.Context, _ *drand.GroupRequest) (*drand.GroupPacket, error) {
	_, span := tracer.NewSpan(ctx, "bp.GroupFile")
	defer span.End()

	bp.state.RLock()
	defer bp.state.RUnlock()

	if bp.group == nil {
		return nil, ErrNoGroupSetup
	}

	protoGroup := bp.group.ToProto(bp.version)

	return protoGroup, nil
}

// BackupDatabase triggers a backup of the primary database.
func (bp *BeaconProcess) BackupDatabase(ctx context.Context, req *drand.BackupDBRequest) (*drand.BackupDBResponse, error) {
	ctx, span := tracer.NewSpan(ctx, "bp.BackupDatabase")
	defer span.End()

	bp.state.RLock()
	if bp.beacon == nil {
		bp.state.RUnlock()
		return nil, errors.New("drand: beacon not setup yet")
	}
	inst := bp.beacon
	bp.state.RUnlock()

	w, err := fs.CreateSecureFile(req.OutputFile)
	if err != nil {
		return nil, fmt.Errorf("could not open file for backup: %w", err)
	}
	defer w.Close()

	return &drand.BackupDBResponse{Metadata: bp.newMetadata()}, inst.Store().SaveTo(ctx, w)
}

// PingPong simply responds with an empty packet, proving that this drand node
// is up and alive.
func (bp *BeaconProcess) PingPong(ctx context.Context, _ *drand.Ping) (*drand.Pong, error) {
	_, span := tracer.NewSpan(ctx, "bp.Ping")
	defer span.End()

	return &drand.Pong{Metadata: bp.newMetadata()}, nil
}

func (bp *BeaconProcess) RemoteStatus(ctx context.Context, in *drand.RemoteStatusRequest) (*drand.RemoteStatusResponse, error) {
	ctx, span := tracer.NewSpan(ctx, "bp.RemoteStatus")
	defer span.End()

	replies := make(map[string]*drand.StatusResponse)
	nodes := in.GetAddresses()
	if len(nodes) == 0 {
		if bp.beacon != nil && bp.group != nil {
			for _, node := range bp.group.Nodes {
				if node.Addr == bp.priv.Public.Addr {
					continue
				}

				nodes = append(nodes, &drand.Address{Address: node.Address()})
			}
		}
	}
	bp.log.Debugw("Starting remote status request", "for_nodes", nodes)
	for _, addr := range nodes {
		remoteAddress := addr.GetAddress()
		if remoteAddress == "" {
			bp.log.Errorw("Received empty address during remote status", "addr", addr)
			continue
		}

		var err error
		var resp *drand.StatusResponse
		statusReq := &drand.StatusRequest{
			CheckConn: nodes,
			Metadata:  bp.newMetadata(),
		}
		if remoteAddress == bp.priv.Public.Addr {
			// it's ourself
			resp, err = bp.Status(ctx, statusReq)
		} else {
			bp.log.Debugw("Sending status request", "for_node", remoteAddress)
			p := net.CreatePeer(remoteAddress)
			resp, err = bp.privGateway.Status(ctx, p, statusReq)
		}
		if err != nil {
			bp.log.Warnw("Status request failed", "for_node", addr, "error", err)
		} else {
			replies[remoteAddress] = resp
		}
	}

	bp.log.Debugw("Done with remote status request", "replies_length", len(replies))
	return &drand.RemoteStatusResponse{
		Statuses: replies,
	}, nil
}

// Status responds with the actual status of drand process
func (bp *BeaconProcess) Status(ctx context.Context, in *drand.StatusRequest) (*drand.StatusResponse, error) {
	ctx, span := tracer.NewSpan(ctx, "bp.Status")
	defer span.End()

	bp.state.RLock()
	defer bp.state.RUnlock()

	bp.log.Debugw("Processing incoming Status request")

	dkgStatus := drand.DkgStatus{}
	beaconStatus := drand.BeaconStatus{}
	chainStore := drand.ChainStoreStatus{}

	// Beacon status
	beaconStatus.Status = uint32(BeaconNotInited)
	chainStore.IsEmpty = true

	if bp.beacon != nil {
		beaconStatus.Status = uint32(BeaconInited)

		beaconStatus.IsStopped = bp.beacon.IsStopped()
		beaconStatus.IsRunning = bp.beacon.IsRunning()
		beaconStatus.IsServing = bp.beacon.IsServing()

		// Chain store
		lastBeacon, err := bp.beacon.Store().Last(ctx)

		if err == nil && lastBeacon != nil {
			chainStore.IsEmpty = false
			chainStore.LastStored = lastBeacon.GetRound()
			chainStore.ExpectedLast = common.CurrentRound(bp.opts.clock.Now().Unix(), bp.group.Period, bp.group.GenesisTime)
		}
	}

	// remote network connectivity
	nodeList := in.GetCheckConn()
	// in case of a remote nodelist made of only ourself, instead we test all nodes in the group file
	if len(nodeList) == 1 && nodeList[0].Address == bp.priv.Public.Addr && bp.beacon != nil && bp.group != nil {
		bp.log.Debugw("Empty node connectivity list, populating with group file")
		for _, node := range bp.group.Nodes {
			nodeList = append(nodeList, &drand.Address{Address: node.Address()})
		}
	}

	bp.log.Debugw("Starting remote network connectivity check", "for_nodes", nodeList)
	resp := make(map[string]bool)
	for _, addr := range nodeList {
		remoteAddress := addr.GetAddress()
		if remoteAddress == "" {
			bp.log.Warnw("Skipping empty address", "addr", addr)
			continue
		}
		if remoteAddress == bp.priv.Public.Addr {
			// Skipping ourselves for the connectivity test
			continue
		}

		p := net.CreatePeer(remoteAddress)
		// we use an anonymous function to not leak the defer in the for loop
		func() {
			ctx, span := tracer.NewSpan(ctx, "bp.Status.sendingHome")
			span.SetAttributes(attribute.String("nodeAddr", remoteAddress))
			defer span.End()

			// Simply try to ping him see if he replies
			tc, cancel := context.WithTimeout(ctx, callMaxTimeout)
			defer cancel()
			bp.log.Debugw("Sending Check request", "for_node", remoteAddress)
			err := bp.privGateway.Check(tc, p)
			if err != nil {
				bp.log.Debugw("Status request failed", "remote", addr, "error", err)
				resp[remoteAddress] = false
			} else {
				resp[remoteAddress] = true
			}
		}()
	}
	bp.log.Debugw("Done with connectivity check", "response_length", len(resp))

	packet := &drand.StatusResponse{
		Dkg:        &dkgStatus,
		ChainStore: &chainStore,
		Beacon:     &beaconStatus,
	}
	if len(resp) > 0 {
		packet.Connections = resp
	}
	return packet, nil
}

func (bp *BeaconProcess) ListSchemes(ctx context.Context, _ *drand.ListSchemesRequest) (*drand.ListSchemesResponse, error) {
	_, span := tracer.NewSpan(ctx, "bp.ListSchemes")
	defer span.End()

	return &drand.ListSchemesResponse{Ids: crypto.ListSchemes(), Metadata: bp.newMetadata()}, nil
}

func (bp *BeaconProcess) ListBeaconIDs(ctx context.Context, _ *drand.ListBeaconIDsRequest) (*drand.ListBeaconIDsResponse, error) {
	_, span := tracer.NewSpan(ctx, "bp.ListBeaconIDs")
	defer span.End()

	return nil, fmt.Errorf("method bp.ListBeaconIDs not implemented")
}

// StartFollowChain syncs up with a chain from other nodes
//
//nolint:funlen,gocyclo,lll
func (bp *BeaconProcess) StartFollowChain(ctx context.Context, req *drand.StartSyncRequest, stream drand.Control_StartFollowChainServer) error {
	ctx, span := tracer.NewSpan(ctx, "bp.StartFollowChain")
	defer span.End()

	// TODO replace via a more independent chain manager that manages the
	// transition from following -> participating
	bp.state.Lock()
	logger := bp.log.Named("Follow")
	if bp.syncerCancel != nil {
		bp.state.Unlock()
		err := errors.New("syncing is already in progress")
		logger.Debugw("beacon_process", "err", err)
		return err
	}

	// context given to the syncer
	// NOTE: this means that if the client quits the requests, the syncing
	// context will signal and it will stop. If we want the following to
	// continue nevertheless we can use the next line by using a new context.
	// ctx, cancel := context.WithCancel(context.Background())
	ctx, cancel := context.WithCancel(ctx)
	bp.syncerCancel = cancel
	bp.state.Unlock()

	defer func() {
		bp.state.Lock()
		if bp.syncerCancel != nil {
			// it can be nil when we recreate a new beacon we cancel it
			// see drand.go:newBeacon()
			bp.syncerCancel()
		}
		bp.syncerCancel = nil
		bp.state.Unlock()
	}()

	peers := make([]net.Peer, 0, len(req.GetNodes()))
	for _, addr := range req.GetNodes() {
		// we skip our own address
		if addr == bp.priv.Public.Address() {
			continue
		}
		peers = append(peers, net.CreatePeer(addr))
	}

	info, err := bp.chainInfoFromPeers(ctx, peers)
	if err != nil {
		return err
	}

	// we need to get the beaconID from the request since we follow a chain we might not know yet
	hash := req.GetMetadata().GetChainHash()
	if !bytes.Equal(info.Hash(), hash) {
		return fmt.Errorf("chain hash mismatch: rcv(%x) != bp(%x)", info.Hash(), hash)
	}

	logger.Debugw("", "start_follow_chain", "fetched chain info", "hash", fmt.Sprintf("%x", info.GenesisSeed))

	if !common.CompareBeaconIDs(bp.getBeaconID(), info.ID) {
		return errors.New("invalid beacon id on chain info")
	}

	store, err := bp.createDBStore(context.Background())
	if err != nil {
		logger.Errorw("", "start_follow_chain", "unable to create store", "err", err)
		return fmt.Errorf("unable to create store: %w", err)
	}

	// TODO find a better place to put that
	if err := store.Put(ctx, chain.GenesisBeacon(info.GenesisSeed)); err != nil {
		logger.Errorw("", "start_follow_chain", "unable to insert genesis block", "err", err)
		store.Close()
		return fmt.Errorf("unable to insert genesis block: %w", err)
	}

	// add sch store to handle sch configuration on beacon storing process correctly
	sch, err := crypto.SchemeFromName(info.GetSchemeName())
	if err != nil {
		return err
	}
	ss, err := beacon.NewSchemeStore(ctx, store, sch)
	if err != nil {
		return err
	}

	// register callback to notify client of progress
	cbStore := beacon.NewCallbackStore(bp.log, ss)
	defer cbStore.Close()

	cb, done := bp.sendProgressCallback(ctx, stream, req.GetUpTo(), info, bp.opts.clock)

	addr := net.RemoteAddress(stream.Context())
	cbStore.AddCallback(addr, cb)
	defer cbStore.RemoveCallback(addr)

	syncer, err := beacon.NewSyncManager(ctx, &beacon.SyncConfig{
		Log:         logger,
		Store:       cbStore,
		BoltdbStore: store,
		Info:        info,
		Client:      bp.privGateway,
		Clock:       bp.opts.clock,
		NodeAddr:    bp.priv.Public.Address(),
	})
	if err != nil {
		return err
	}

	go syncer.Run()
	defer syncer.Stop()

	logger.Debugw("Launching follow now")
	var errChan chan error

	for {
		syncCtx, syncCancel := context.WithCancel(ctx)
		go func() {
			errChan <- syncer.Sync(syncCtx, beacon.NewRequestInfo(ctx, req.GetUpTo(), peers))
		}() // wait for all the callbacks to be called and progress sent before returning
		select {
		case <-done:
			syncCancel()
			return nil
		case <-ctx.Done():
			syncCancel()
			return ctx.Err()
		case <-errChan:
			syncCancel()
			logger.Errorw("Error while trying to follow chain, trying again in 2 periods")
			// in case of error we retry after a period elapsed, since follow must run until canceled
			time.Sleep(info.Period)
			continue
		}
	}
}

// StartCheckChain checks a chain for validity and pulls invalid beacons from other nodes
func (bp *BeaconProcess) StartCheckChain(req *drand.StartSyncRequest, stream drand.Control_StartCheckChainServer) error {
	ctx := stream.Context()
	ctx, span := tracer.NewSpan(ctx, "bp.StartCheckChain")
	defer span.End()

	logger := bp.log.Named("CheckChain")

	if bp.beacon == nil {
		return errors.New("beacon handler is nil, you might need to first --follow a chain and start aggregating beacons")
	}

	logger.Infow("Starting to check chain for invalid beacons")

	bp.state.Lock()
	if bp.syncerCancel != nil {
		bp.state.Unlock()
		return errors.New("syncing is already in progress")
	}
	// context given to the syncer
	// NOTE: this means that if the client quits the requests, the syncing
	// context will signal it, and it will stop.
	ctx, cancel := context.WithCancel(ctx)
	bp.syncerCancel = cancel
	bp.state.Unlock()
	defer func() {
		bp.state.Lock()
		if bp.syncerCancel != nil {
			bp.syncerCancel()
		}
		bp.syncerCancel = nil
		bp.state.Unlock()
	}()

	// we don't monitor the channel for this one, instead we'll error out if needed
	cb, _ := bp.sendPlainProgressCallback(ctx, stream, false)

	peers := make([]net.Peer, 0, len(req.GetNodes()))
	for _, addr := range req.GetNodes() {
		// we skip our own address
		if addr == bp.priv.Public.Address() {
			continue
		}
		// TODO add TLS disable later
		peers = append(peers, net.CreatePeer(addr))
	}

	logger.Debugw("validate_and_sync", "up_to", req.UpTo)
	faultyBeacons, err := bp.beacon.ValidateChain(ctx, req.UpTo, cb)
	if err != nil {
		return err
	}

	// let us reset the progress bar on the client side to track instead the progress of the correction on the beacons
	// this will also pass the "invalid beacon count" to the client through the new target.
	err = stream.Send(&drand.SyncProgress{
		Current: 0,
		Target:  uint64(len(faultyBeacons)),
	})
	if err != nil {
		logger.Errorw("", "send_progress", "sending_progress", "err", err)
	}

	// if we're asking to sync against only us, it's a dry-run
	dryRun := len(req.Nodes) == 1 && req.Nodes[0] == bp.priv.Public.Addr
	if len(faultyBeacons) == 0 || dryRun {
		logger.Infow("Finished without taking any corrective measure", "amount_invalid", len(faultyBeacons), "dry_run", dryRun)
		return nil
	}

	// We need to wait a bit before continuing sending to the client if we don't want mingled output on the client side.
	time.Sleep(time.Second)

	// we need the channel to make sure the client has received the progress
	cb, done := bp.sendPlainProgressCallback(ctx, stream, false)

	logger.Infow("Faulty beacons detected in chain, correcting now", "dry-run", false)
	logger.Debugw("Faulty beacons", "List", faultyBeacons)

	err = bp.beacon.CorrectChain(ctx, faultyBeacons, peers, cb)
	if err != nil {
		return err
	}

	// wait for all the callbacks to be called and progress sent before returning
	select {
	case <-done:
		logger.Debugw("Finished correcting chain successfully", "up_to", req.UpTo)
		return nil
	case <-ctx.Done():
		logger.Errorw("Received a cancellation / stream closed", "err", ctx.Err())
		return ctx.Err()
	}
}

// chainInfoFromPeers attempts to fetch chain info from one of the passed peers.
func (bp *BeaconProcess) chainInfoFromPeers(ctx context.Context, peers []net.Peer) (*public.Info, error) {
	ctx, span := tracer.NewSpan(ctx, "bp.chainInfoFromPeers")
	defer span.End()

	bp.state.RLock()
	privGateway := bp.privGateway
	logger := bp.log.Named("InfoFromPeers")
	version := bp.version
	beaconID := bp.beaconID
	bp.state.RUnlock()

	// we first craft our request
	request := new(drand.ChainInfoRequest)
	request.Metadata = &drand.Metadata{BeaconID: beaconID, NodeVersion: version.ToProto()}

	var info *public.Info
	var err error
	for _, peer := range peers {
		var ci *drand.ChainInfoPacket
		ci, err = privGateway.ChainInfo(ctx, peer, request)
		if err != nil {
			logger.Errorw("", "start_follow_chain", "error getting chain info", "from", peer.Address(), "err", err)
			continue
		}
		info, err = public.InfoFromProto(ci)
		if err != nil {
			logger.Errorw("", "start_follow_chain", "invalid chain info", "from", peer.Address(), "err", err)
			continue
		}
	}
	if info == nil {
		return nil, fmt.Errorf("unable to get chain info successfully. Last err: %w", err)
	}
	return info, nil
}

// sendProgressCallback returns a function that sends SyncProgress on the
// passed stream. It also returns a channel that closes when the callback is
// called with a beacon whose round matches the passed upTo value.
func (bp *BeaconProcess) sendProgressCallback(
	ctx context.Context,
	stream drand.Control_StartFollowChainServer,
	upTo uint64, info *public.Info,
	clk clock.Clock,
) (cb beacon.CallbackFunc, done chan struct{}) {
	ctx, span := tracer.NewSpan(ctx, "bp.StartCheckChain")
	defer span.End()

	targ := common.CurrentRound(clk.Now().Unix(), info.Period, info.GenesisTime)
	if upTo != 0 && upTo < targ {
		targ = upTo
	}

	var plainProgressCb func(a, b uint64)
	plainProgressCb, done = bp.sendPlainProgressCallback(ctx, stream, upTo == 0)
	cb = func(b *common.Beacon, closed bool) {
		if closed {
			return
		}

		plainProgressCb(b.Round, targ)
	}

	return
}

// sendPlainProgressCallback returns a function that sends SyncProgress on the
// passed stream. It also returns a channel that closes when the callback is
// called with a value whose round matches the passed upTo value.
func (bp *BeaconProcess) sendPlainProgressCallback(ctx context.Context,
	stream drand.Control_StartFollowChainServer,
	keepFollowing bool,
) (cb func(curr uint64, targ uint64), done chan struct{}) {
	_, span := tracer.NewSpan(ctx, "bp.sendPlainProgressCallback")
	defer span.End()

	done = make(chan struct{})
	logger := bp.log.Named("ProgressCB")
	cb = func(curr, targ uint64) {
		// avoids wrapping below and sends latest round number to the client
		if curr > targ {
			targ = curr
		}
		// let us do some rate limiting on the amount of Send we do
		if targ > common.LogsToSkip && targ-curr > common.LogsToSkip && curr%common.LogsToSkip != 0 {
			return
		}

		err := stream.Send(&drand.SyncProgress{
			Current: curr,
			Target:  targ,
		})
		if err != nil {
			logger.Errorw("sending_progress", "err", err)
		}
		if !keepFollowing && targ > 0 && curr == targ {
			logger.Debugw("target reached", "target", curr)
			close(done)
		}
	}
	//nolint:nakedret
	return
}

func (bp *BeaconProcess) validateGroupTransition(oldGroup, newGroup *key.Group) error {
	// theoretically this shouldn't happen under normal use,
	// though if it does, we can safely transition to a new group file
	// also is useful during v1->v2 migration tests
	if oldGroup == nil {
		if newGroup == nil {
			return errors.New("the new group file could not be transitioned to, as it was `nil`")
		}
		return nil
	}
	if oldGroup.GenesisTime != newGroup.GenesisTime {
		bp.log.Errorw("", "setup_reshare", "invalid genesis time in received group")
		return errors.New("control: old and new group have different genesis time")
	}

	if oldGroup.Period != newGroup.Period {
		bp.log.Errorw("", "setup_reshare", "invalid period time in received group")
		return errors.New("control: old and new group have different period - unsupported feature at the moment")
	}

	if !common.CompareBeaconIDs(oldGroup.ID, newGroup.ID) {
		bp.log.Errorw("", "setup_reshare", "invalid ID in received group")
		return errors.New("control: old and new group have different ID - unsupported feature at the moment")
	}

	if !bytes.Equal(oldGroup.GetGenesisSeed(), newGroup.GetGenesisSeed()) {
		bp.log.Errorw("", "setup_reshare", "invalid genesis seed in received group")
		return errors.New("control: old and new group have different genesis seed")
	}
	now := bp.opts.clock.Now().Unix()
	if newGroup.TransitionTime < now {
		bp.log.Errorw("", "setup_reshare", "invalid_transition", "given", newGroup.TransitionTime, "now", now)
		return errors.New("control: new group with transition time in the past")
	}
	return nil
}
