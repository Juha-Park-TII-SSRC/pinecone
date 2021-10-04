package router

import (
	"crypto/ed25519"
	"crypto/rand"
	"fmt"
	"time"

	"github.com/matrix-org/pinecone/types"
	"github.com/matrix-org/pinecone/util"
)

const virtualSnakeMaintainInterval = time.Second

const virtualSnakeNeighExpiryPeriod = time.Hour

type virtualSnakeTable map[virtualSnakeIndex]*virtualSnakeEntry

type virtualSnakeIndex struct {
	PublicKey types.PublicKey
	PathID    types.VirtualSnakePathID
}

type virtualSnakeEntry struct {
	PublicKey     types.PublicKey
	PathID        types.VirtualSnakePathID
	Source        *peer
	Destination   *peer
	LastSeen      time.Time
	RootPublicKey types.PublicKey
	RootSequence  types.Varu64
}

func (s *state) _maintainSnake() {
	select {
	case <-s.r.context.Done():
		return
	default:
	}

	rootAnn := s._rootAnnouncement()
	canBootstrap := s._parent != nil && rootAnn.RootPublicKey != s.r.public
	willBootstrap := false

	if asc := s._ascending; asc != nil {
		switch {
		case time.Since(asc.LastSeen) >= virtualSnakeNeighExpiryPeriod:
			s._sendTeardownForPath(asc.PublicKey, asc.PathID, nil, true)
		case asc.RootPublicKey != rootAnn.RootPublicKey || asc.RootSequence != rootAnn.Sequence:
			willBootstrap = canBootstrap
		}
	} else {
		willBootstrap = canBootstrap
	}

	if desc := s._descending; desc != nil {
		switch {
		case time.Since(desc.LastSeen) >= virtualSnakeNeighExpiryPeriod:
			s._sendTeardownForPath(desc.PublicKey, desc.PathID, nil, false)
		case desc.RootPublicKey != rootAnn.RootPublicKey || desc.RootSequence != rootAnn.Sequence:
			//s._sendTeardownForPath(desc.PublicKey, desc.PathID, nil, false)
		}
	}

	// Send bootstrap messages into the network. Ordinarily we
	// would only want to do this when starting up or after a
	// predefined interval, but for now we'll continue to send
	// them on a regular interval until we can derive some better
	// connection state.
	if willBootstrap {
		s._bootstrapNow()
	}

	s._maintainSnakeIn(virtualSnakeMaintainInterval)
}

func (s *state) _bootstrapNow() {
	ann := s._rootAnnouncement()
	payload := make([]byte, 8+ed25519.PublicKeySize+ann.Sequence.Length())
	bootstrap := types.VirtualSnakeBootstrap{
		RootPublicKey: ann.RootPublicKey,
		RootSequence:  ann.Sequence,
	}
	if _, err := rand.Read(bootstrap.PathID[:]); err != nil {
		return
	}
	if _, err := bootstrap.MarshalBinary(payload[:]); err != nil {
		return
	}
	send := &types.Frame{
		Type:           types.TypeVirtualSnakeBootstrap,
		DestinationKey: s.r.public,
		Source:         s._coords(),
		Payload:        payload[:],
	}
	for _, p := range s._nextHopsSNEK(s.r.local, send, true) {
		if p.proto.push(send) {
			return
		}
	}
}

func (s *state) _nextHopsSNEK(from *peer, rx *types.Frame, bootstrap bool) []*peer {
	destKey := rx.DestinationKey
	if !bootstrap && s.r.public.EqualTo(destKey) {
		return []*peer{s.r.local}
	}
	rootAnn := s._rootAnnouncement()
	rootKey := rootAnn.RootPublicKey
	ancestors, parentPort := s._ancestors()
	if len(ancestors) > 0 {
		rootKey = ancestors[0]
		ancestors = ancestors[1:]
	}
	bestKey := s.r.public
	bestPeer := s.r.local
	newCandidate := func(key types.PublicKey, p *peer) {
		bestKey, bestPeer = key, p
	}
	newCheckedCandidate := func(candidate types.PublicKey, p *peer) {
		switch {
		case !bootstrap && candidate.EqualTo(destKey) && !bestKey.EqualTo(destKey):
			newCandidate(candidate, p)
		case util.DHTOrdered(destKey, candidate, bestKey):
			newCandidate(candidate, p)
		}
	}

	// Check if we can use the path to the root as a starting point
	switch {
	case bootstrap && bestKey.EqualTo(destKey):
		// Bootstraps always start working towards the root so that
		// they go somewhere rather than getting stuck
		newCandidate(rootKey, parentPort)
	case destKey.EqualTo(rootKey):
		// The destination is actually the root node itself
		newCandidate(rootKey, parentPort)
	case util.DHTOrdered(bestKey, destKey, rootKey):
		// The destination key is higher than our own key, so
		// start using the path to the root as the first candidate
		newCandidate(rootKey, parentPort)
	}

	// Check our direct ancestors
	// bestKey <= destKey < rootKey
	for _, ancestor := range ancestors {
		newCheckedCandidate(ancestor, parentPort)
	}

	// These conditions guard from selecting obviously bad peers
	peerValid := func(p *peer) bool {
		if p == nil || !p.started.Load() {
			return false
		}
		if s._announcements[p] == nil {
			return false
		}
		return true
	}

	// Check our direct peers ancestors
	for _, p := range s._peers {
		if !peerValid(p) {
			continue
		}
		for _, hop := range s._announcements[p].Signatures {
			newCheckedCandidate(hop.PublicKey, p)
		}
	}

	// Check our direct peers
	for _, p := range s._peers {
		if !peerValid(p) {
			continue
		}
		if peerKey := p.public; bestKey.EqualTo(peerKey) {
			// We've seen this key already, either as one of our ancestors
			// or as an ancestor of one of our peers, but it turns out we
			// are directly peered with that node, so use the more direct
			// path instead
			newCandidate(peerKey, p)
		}
	}

	// Check our DHT entries
	for dhtKey, entry := range s._table {
		switch {
		case time.Since(entry.LastSeen) >= virtualSnakeNeighExpiryPeriod:
			continue
		default:
			newCheckedCandidate(dhtKey.PublicKey, entry.Source)
		}
	}

	// Return the candidate ports
	if bestPeer != nil {
		return []*peer{bestPeer}
	}
	return []*peer{}
}

func (s *state) _handleBootstrap(from *peer, rx *types.Frame) error {
	if rx.DestinationKey.EqualTo(s.r.public) {
		return nil
	}
	// Unmarshal the bootstrap.
	var bootstrap types.VirtualSnakeBootstrap
	_, err := bootstrap.UnmarshalBinary(rx.Payload)
	if err != nil {
		return fmt.Errorf("bootstrap.UnmarshalBinary: %w", err)
	}
	root := s._rootAnnouncement()
	bootstrapACK := types.VirtualSnakeBootstrapACK{
		PathID:        bootstrap.PathID,
		RootPublicKey: root.RootPublicKey,
		RootSequence:  root.Sequence,
	}
	buf := make([]byte, 8+ed25519.PublicKeySize+root.Sequence.Length())
	if _, err := bootstrapACK.MarshalBinary(buf[:]); err != nil {
		return fmt.Errorf("bootstrapACK.MarshalBinary: %w", err)
	}
	acknowledge := false
	desc := s._descending
	switch {
	case rx.SourceKey.EqualTo(s.r.public):
		// We received a bootstrap from ourselves. This shouldn't happen,
		// so either another node has forwarded it to us incorrectly, or
		// a routing loop has occurred somewhere. Don't act on the bootstrap
		// in that case.
	case !bootstrap.RootPublicKey.EqualTo(root.RootPublicKey) || bootstrap.RootSequence != root.Sequence:
		// The root or sequence don't match so we won't act on the bootstrap.
	case desc != nil && desc.PublicKey.EqualTo(rx.DestinationKey):
		// We've received another bootstrap from our direct descending node.
		// Send back an acknowledgement as this is OK.
		acknowledge = true
	case desc != nil && time.Since(desc.LastSeen) >= virtualSnakeNeighExpiryPeriod:
		// We already have a direct descending node, but we haven't seen it
		// recently, so it's quite possible that it has disappeared. We'll
		// therefore handle this bootstrap instead. If the original node comes
		// back later and is closer to us then we'll end up using it again.
		acknowledge = true
	case desc == nil && util.LessThan(rx.DestinationKey, s.r.public):
		// We don't know about a descending node and at the moment we don't know
		// any better candidates, so we'll accept a bootstrap from a node with a
		// key lower than ours (so that it matches descending order).
		acknowledge = true
	case desc != nil && util.DHTOrdered(desc.PublicKey, rx.DestinationKey, s.r.public):
		// We know about a descending node already but it turns out that this
		// new node that we've received a bootstrap from is actually closer to
		// us than the previous node. We'll update our record to use the new
		// node instead and then send back a bootstrap ACK.
		acknowledge = true
	default:
		// The bootstrap conditions weren't met. This might just be because
		// there's a node out there that hasn't converged to a closer node
		// yet, so we'll just ignore the bootstrap.
	}
	if acknowledge {
		send := &types.Frame{
			Destination:    rx.Source,
			DestinationKey: rx.DestinationKey,
			Source:         s._coords(),
			SourceKey:      s.r.public,
			Type:           types.TypeVirtualSnakeBootstrapACK,
			Payload:        buf[:],
		}
		for _, p := range s._nextHopsTree(s.r.local, send) {
			if p.proto.push(send) {
				return nil
			}
		}
		return nil
	}
	return nil
}

func (s *state) _handleBootstrapACK(from *peer, rx *types.Frame) error {
	// Unmarshal the bootstrap ACK.
	var bootstrapACK types.VirtualSnakeBootstrapACK
	_, err := bootstrapACK.UnmarshalBinary(rx.Payload)
	if err != nil {
		return fmt.Errorf("bootstrapACK.UnmarshalBinary: %w", err)
	}
	root := s._rootAnnouncement()
	update := false
	asc := s._ascending
	switch {
	case rx.SourceKey.EqualTo(s.r.public):
		// We received a bootstrap ACK from ourselves. This shouldn't happen,
		// so either another node has forwarded it to us incorrectly, or
		// a routing loop has occurred somewhere. Don't act on the bootstrap
		// in that case.
	case !bootstrapACK.RootPublicKey.EqualTo(root.RootPublicKey) || bootstrapACK.RootSequence != root.Sequence:
		// The root or sequence don't match so we won't act on the bootstrap.
	case asc != nil && asc.PublicKey.EqualTo(rx.SourceKey) && asc.PathID != bootstrapACK.PathID:
		// We've received another bootstrap ACK from our direct ascending node.
		// Just refresh the record and then send a new path setup message to
		// that node.
		update = true
	case asc != nil && time.Since(asc.LastSeen) >= virtualSnakeNeighExpiryPeriod:
		// We already have a direct ascending node, but we haven't seen it
		// recently, so it's quite possible that it has disappeared. We'll
		// therefore handle this bootstrap ACK instead. If the original node comes
		// back later and is closer to us then we'll end up using it again.
		update = true
	case asc == nil && util.LessThan(s.r.public, rx.SourceKey):
		// We don't know about an ascending node and at the moment we don't know
		// any better candidates, so we'll accept a bootstrap ACK from a node with a
		// key higher than ours (so that it matches descending order).
		update = true
	case asc != nil && util.DHTOrdered(s.r.public, rx.SourceKey, asc.PublicKey):
		// We know about an ascending node already but it turns out that this
		// new node that we've received a bootstrap from is actually closer to
		// us than the previous node. We'll update our record to use the new
		// node instead and then send a new path setup message to it.
		update = true
	default:
		// The bootstrap ACK conditions weren't met. This might just be because
		// there's a node out there that hasn't converged to a closer node
		// yet, so we'll just ignore the acknowledgement.
	}
	if update {
		if asc != nil {
			// Remote side is responsible for clearing up the replaced path, but
			// we do want to make sure we don't have any old paths to other nodes
			// that *aren't* the new ascending node lying around.
			s._sendTeardownForPath(asc.PublicKey, asc.PathID, nil, true)
		}
		entry := &virtualSnakeEntry{
			PublicKey:     rx.SourceKey,
			Source:        from,
			Destination:   s.r.local,
			LastSeen:      time.Now(),
			PathID:        bootstrapACK.PathID,
			RootPublicKey: bootstrapACK.RootPublicKey,
			RootSequence:  bootstrapACK.RootSequence,
		}
		/*
			index := virtualSnakeIndex{
				PublicKey: rx.SourceKey,
				PathID:    bootstrapACK.PathID,
			}
			s._table[index] = entry
		*/
		s._ascending = entry
		setup := types.VirtualSnakeSetup{ // nolint:gosimple
			PathID:        bootstrapACK.PathID,
			RootPublicKey: root.RootPublicKey,
			RootSequence:  root.Sequence,
		}
		buf := make([]byte, 8+ed25519.PublicKeySize+root.Sequence.Length())
		if _, err := setup.MarshalBinary(buf[:]); err != nil {
			return fmt.Errorf("setup.MarshalBinary: %w", err)
		}
		ts, err := util.SignedTimestamp(s.r.private)
		if err != nil {
			return fmt.Errorf("util.SignedTimestamp: %w", err)
		}
		send := &types.Frame{
			Destination:    rx.Source,
			DestinationKey: rx.SourceKey, // the other end of the path
			SourceKey:      s.r.public,   // our source key
			Type:           types.TypeVirtualSnakeSetup,
			Payload:        append(buf[:], ts...),
		}
		for _, p := range s._nextHopsTree(s.r.local, send) {
			if p.proto.push(send) {
				return nil
			}
		}
		return nil
	}
	return nil
}

func (s *state) _handleSetup(from *peer, rx *types.Frame, nextHops []*peer) error {
	root := s._rootAnnouncement()

	// Unmarshal the setup.
	var setup types.VirtualSnakeSetup
	if _, err := setup.UnmarshalBinary(rx.Payload); err != nil {
		return fmt.Errorf("setup.UnmarshalBinary: %w", err)
	}

	// Did the setup hit a dead end on the way to the ascending node?
	if (len(nextHops) == 0 || nextHops[0] == s.r.local) && !rx.DestinationKey.EqualTo(s.r.public) {
		s._sendTeardownForPath(rx.SourceKey, setup.PathID, from, false)
		return nil
	}

	var addToRoutingTable bool

	// Is the setup a duplicate of one we already have in our table?
	if _, ok := s._table[virtualSnakeIndex{rx.SourceKey, setup.PathID}]; ok {
		s._sendTeardownForPath(rx.SourceKey, setup.PathID, nil, false)  // first call fixes routing table
		s._sendTeardownForPath(rx.SourceKey, setup.PathID, from, false) // second call sends back to origin
		return fmt.Errorf("setup is a duplicate")
	}

	// If we're at the destination of the setup then update our predecessor
	// with information from the bootstrap.
	if rx.DestinationKey.EqualTo(s.r.public) {
		update := false
		desc := s._descending
		switch {
		case rx.SourceKey.EqualTo(s.r.public):
			// We received a bootstrap from ourselves. This shouldn't happen,
			// so either another node has forwarded it to us incorrectly, or
			// a routing loop has occurred somewhere. Don't act on the bootstrap
			// in that case.
		case !setup.RootPublicKey.EqualTo(root.RootPublicKey) || setup.RootSequence != root.Sequence:
			// The root or sequence don't match so we won't act on the setup
			// and send a teardown back to the sender.
		case desc != nil && desc.PublicKey.EqualTo(rx.SourceKey):
			// We've received another bootstrap from our direct descending node.
			// Just refresh the record and then send back an acknowledgement.
			update = true
		case desc != nil && time.Since(desc.LastSeen) >= virtualSnakeNeighExpiryPeriod:
			// We already have a direct descending node, but we haven't seen it
			// recently, so it's quite possible that it has disappeared. We'll
			// therefore handle this bootstrap instead. If the original node comes
			// back later and is closer to us then we'll end up using it again.
			update = true
		case desc == nil && util.LessThan(rx.SourceKey, s.r.public):
			// We don't know about a descending node and at the moment we don't know
			// any better candidates, so we'll accept a bootstrap from a node with a
			// key lower than ours (so that it matches descending order).
			update = true
		case desc != nil && util.DHTOrdered(desc.PublicKey, rx.SourceKey, s.r.public):
			// We know about a descending node already but it turns out that this
			// new node that we've received a bootstrap from is actually closer to
			// us than the previous node. We'll update our record to use the new
			// node instead and then send back a bootstrap ACK.
			update = true
		default:
			// The bootstrap conditions weren't met. This might just be because
			// there's a node out there that hasn't converged to a closer node
			// yet, so we'll just ignore the bootstrap.
		}
		if update {
			if desc != nil {
				// Tear down the previous path, if there was one.
				s._sendTeardownForPath(desc.PublicKey, desc.PathID, nil, false)
			}
			entry := &virtualSnakeEntry{
				PublicKey:     rx.SourceKey,
				Source:        from,
				Destination:   s.r.local,
				LastSeen:      time.Now(),
				PathID:        setup.PathID,
				RootPublicKey: setup.RootPublicKey,
				RootSequence:  setup.RootSequence,
			}
			index := virtualSnakeIndex{
				PublicKey: rx.SourceKey,
				PathID:    setup.PathID,
			}
			s._table[index] = entry
			s._descending = entry
			return nil
		}
	} else {
		addToRoutingTable = true
	}

	if addToRoutingTable {
		// Add a new routing table entry.
		// TODO: The routing table needs to be bounded by size, so that we don't
		// exhaust available system memory trying to maintain network paths. To
		// bound the routing table safely, we may want to make sure that we have
		// a reasonable spread of routes across keyspace so that we don't create
		// any obvious routing holes.
		index := virtualSnakeIndex{
			PublicKey: rx.SourceKey,
			PathID:    setup.PathID,
		}
		entry := &virtualSnakeEntry{
			LastSeen:      time.Now(),
			RootPublicKey: setup.RootPublicKey,
			RootSequence:  setup.RootSequence,
			Source:        from,      // node with lower of the two keys
			Destination:   s.r.local, // will be replaced next
		}
		if len(nextHops) > 0 {
			entry.Destination = nextHops[0] // node with higher of the two keys
		}
		s._table[index] = entry

		return nil
	}

	s._sendTeardownForPath(rx.SourceKey, setup.PathID, from, false)
	return nil
}

func (s *state) _handleTeardown(from *peer, rx *types.Frame) (*peer, error) {
	if len(rx.Payload) < 8 {
		return nil, fmt.Errorf("payload too short")
	}
	var teardown types.VirtualSnakeTeardown
	if _, err := teardown.UnmarshalBinary(rx.Payload); err != nil {
		return nil, fmt.Errorf("teardown.UnmarshalBinary: %w", err)
	}
	return s._teardownPath(from, rx.DestinationKey, teardown.PathID), nil
}

func (s *state) _sendTeardownForPath(pathKey types.PublicKey, pathID types.VirtualSnakePathID, via *peer, ascending bool) {
	// If we're cleaning our "ascending" node then _getTeardown will return a
	// frame which contains our own path key as the source address, because
	// other nodes on the path will know the path by this key. However we need
	// to preserve the original ascending key so that _processsTeardown finds
	// the right path.
	nexthop := s._teardownPath(nil, pathKey, pathID)
	frame := s._getTeardown(pathKey, pathID, ascending)

	switch {
	case via != nil:
		// If "via" is provided, it's because we are tearing down a path that we
		// haven't actually set up or accepted and we need to know where to send
		// the teardown.
		via.proto.push(frame)

	case nexthop != nil:
		// Otherwise, we can only tear down paths that we know about, so if it is,
		// we'll clean up those entries and forward the frame on.
		nexthop.proto.push(frame)
	}
}

func (s *state) _getTeardown(pathKey types.PublicKey, pathID types.VirtualSnakePathID, ascending bool) *types.Frame {
	var payload [8]byte
	teardown := types.VirtualSnakeTeardown{
		PathID: pathID,
	}
	if _, err := teardown.MarshalBinary(payload[:]); err != nil {
		return nil
	}
	if ascending {
		// We're sending a teardown to our ascending node, so the teardown
		// needs to contain *our* key and not theirs, as we're the lower key.
		return &types.Frame{
			Type:           types.TypeVirtualSnakeTeardown,
			DestinationKey: s.r.public,
			Payload:        payload[:],
		}
	}
	return &types.Frame{
		Type:           types.TypeVirtualSnakeTeardown,
		DestinationKey: pathKey,
		Payload:        payload[:],
	}
}

func (s *state) _teardownPath(from *peer, pathKey types.PublicKey, pathID types.VirtualSnakePathID) *peer {
	// Otherwise, we can only tear down paths that we know about, so let's see
	// if it is.
	if asc := s._ascending; asc != nil && asc.PathID == pathID {
		switch {
		case from != nil && s.r.public.EqualTo(pathKey):
			// A teardown coming from the network will contain our public key as
			// the teardown path key.
			fallthrough
		case from == nil && asc.PublicKey.EqualTo(pathKey):
			// A teardown originating locally will contain the remote key as the
			// teardown path key.
			s._ascending = nil
			delete(s._table, virtualSnakeIndex{asc.PublicKey, asc.PathID})
			s._maintainSnakeIn(0)
			return asc.Source
		}
	}
	if desc := s._descending; desc != nil && desc.PublicKey.EqualTo(pathKey) && desc.PathID == pathID {
		s._descending = nil
		delete(s._table, virtualSnakeIndex{desc.PublicKey, desc.PathID})
		return desc.Source
	}
	for k, v := range s._table {
		if k.PublicKey == pathKey && k.PathID == pathID {
			delete(s._table, k)
			switch {
			case from != nil: // the teardown came from the network
				if from == v.Source && v.Destination != s.r.local {
					return v.Destination
				}
				if from == v.Destination && v.Source != s.r.local {
					return v.Source
				}
			case from == nil: // the teardown originated locally
				if v.Source != s.r.local {
					return v.Source
				}
				if v.Destination != s.r.local {
					return v.Destination
				}
			}
		}
	}
	return nil
}
