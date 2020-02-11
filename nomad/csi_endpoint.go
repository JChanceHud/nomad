package nomad

import (
	"fmt"
	"time"

	metrics "github.com/armon/go-metrics"
	log "github.com/hashicorp/go-hclog"
	memdb "github.com/hashicorp/go-memdb"
	multierror "github.com/hashicorp/go-multierror"
	"github.com/hashicorp/nomad/acl"
	cstructs "github.com/hashicorp/nomad/client/structs"
	"github.com/hashicorp/nomad/nomad/state"
	"github.com/hashicorp/nomad/nomad/structs"
)

// CSIVolume wraps the structs.CSIVolume with request data and server context
type CSIVolume struct {
	srv    *Server
	logger log.Logger
}

// QueryACLObj looks up the ACL token in the request and returns the acl.ACL object
// - fallback to node secret ids
func (srv *Server) QueryACLObj(args *structs.QueryOptions) (*acl.ACL, error) {
	if args.AuthToken == "" {
		return nil, fmt.Errorf("authorization required")
	}

	// Lookup the token
	aclObj, err := srv.ResolveToken(args.AuthToken)
	if err != nil {
		// If ResolveToken had an unexpected error return that
		return nil, err
	}

	if aclObj == nil {
		ws := memdb.NewWatchSet()
		node, stateErr := srv.fsm.State().NodeBySecretID(ws, args.AuthToken)
		if stateErr != nil {
			// Return the original ResolveToken error with this err
			var merr multierror.Error
			merr.Errors = append(merr.Errors, err, stateErr)
			return nil, merr.ErrorOrNil()
		}

		if node == nil {
			return nil, structs.ErrTokenNotFound
		}
	}

	return aclObj, nil
}

// WriteACLObj calls QueryACLObj for a WriteRequest
func (srv *Server) WriteACLObj(args *structs.WriteRequest) (*acl.ACL, error) {
	opts := &structs.QueryOptions{
		Region:    args.RequestRegion(),
		Namespace: args.RequestNamespace(),
		AuthToken: args.AuthToken,
	}
	return srv.QueryACLObj(opts)
}

const (
	csiVolumeTable = "csi_volumes"
	csiPluginTable = "csi_plugins"
)

// replySetIndex sets the reply with the last index that modified the table
func (srv *Server) replySetIndex(table string, reply *structs.QueryMeta) error {
	s := srv.fsm.State()

	index, err := s.Index(table)
	if err != nil {
		return err
	}
	reply.Index = index

	// Set the query response
	srv.setQueryMeta(reply)
	return nil
}

// List replies with CSIVolumes, filtered by ACL access
func (v *CSIVolume) List(args *structs.CSIVolumeListRequest, reply *structs.CSIVolumeListResponse) error {
	if done, err := v.srv.forward("CSIVolume.List", args, args, reply); done {
		return err
	}

	aclObj, err := v.srv.QueryACLObj(&args.QueryOptions)
	if err != nil {
		return err
	}

	metricsStart := time.Now()
	defer metrics.MeasureSince([]string{"nomad", "volume", "list"}, metricsStart)

	ns := args.RequestNamespace()
	opts := blockingOptions{
		queryOpts: &args.QueryOptions,
		queryMeta: &reply.QueryMeta,
		run: func(ws memdb.WatchSet, state *state.StateStore) error {
			// Query all volumes
			var err error
			var iter memdb.ResultIterator

			if args.PluginID != "" {
				iter, err = state.CSIVolumesByPluginID(ws, args.PluginID)
			} else {
				iter, err = state.CSIVolumes(ws)
			}

			if err != nil {
				return err
			}

			// Collect results, filter by ACL access
			var vs []*structs.CSIVolListStub
			cache := map[string]bool{}

			for {
				raw := iter.Next()
				if raw == nil {
					break
				}

				vol := raw.(*structs.CSIVolume)
				vol, err := state.CSIVolumeDenormalizePlugins(ws, vol)
				if err != nil {
					return err
				}

				// Filter on the request namespace to avoid ACL checks by volume
				if ns != "" && vol.Namespace != args.RequestNamespace() {
					continue
				}

				// Cache ACL checks QUESTION: are they expensive?
				allowed, ok := cache[vol.Namespace]
				if !ok {
					allowed = aclObj.AllowNsOp(vol.Namespace, acl.NamespaceCapabilityCSIAccess)
					cache[vol.Namespace] = allowed
				}

				if allowed {
					vs = append(vs, vol.Stub())
				}
			}
			reply.Volumes = vs
			return v.srv.replySetIndex(csiVolumeTable, &reply.QueryMeta)
		}}
	return v.srv.blockingRPC(&opts)
}

// Get fetches detailed information about a specific volume
func (v *CSIVolume) Get(args *structs.CSIVolumeGetRequest, reply *structs.CSIVolumeGetResponse) error {
	if done, err := v.srv.forward("CSIVolume.Get", args, args, reply); done {
		return err
	}

	aclObj, err := v.srv.QueryACLObj(&args.QueryOptions)
	if err != nil {
		return err
	}

	if !aclObj.AllowNsOp(args.RequestNamespace(), acl.NamespaceCapabilityCSIAccess) {
		return structs.ErrPermissionDenied
	}

	metricsStart := time.Now()
	defer metrics.MeasureSince([]string{"nomad", "volume", "get"}, metricsStart)

	opts := blockingOptions{
		queryOpts: &args.QueryOptions,
		queryMeta: &reply.QueryMeta,
		run: func(ws memdb.WatchSet, state *state.StateStore) error {
			vol, err := state.CSIVolumeByID(ws, args.ID)
			if err != nil {
				return err
			}

			if vol != nil {
				vol, err = state.CSIVolumeDenormalize(ws, vol)
			}
			if err != nil {
				return err
			}

			reply.Volume = vol
			return v.srv.replySetIndex(csiVolumeTable, &reply.QueryMeta)
		}}
	return v.srv.blockingRPC(&opts)
}

// Register registers a new volume
func (v *CSIVolume) Register(args *structs.CSIVolumeRegisterRequest, reply *structs.CSIVolumeRegisterResponse) error {
	if done, err := v.srv.forward("CSIVolume.Register", args, args, reply); done {
		return err
	}

	aclObj, err := v.srv.WriteACLObj(&args.WriteRequest)
	if err != nil {
		return err
	}

	metricsStart := time.Now()
	defer metrics.MeasureSince([]string{"nomad", "volume", "register"}, metricsStart)

	if !aclObj.AllowNsOp(args.RequestNamespace(), acl.NamespaceCapabilityCSICreateVolume) {
		return structs.ErrPermissionDenied
	}

	// This is the only namespace we ACL checked, force all the volumes to use it
	for _, vol := range args.Volumes {
		vol.Namespace = args.RequestNamespace()
		if err = vol.Validate(); err != nil {
			return err
		}
	}

	resp, index, err := v.srv.raftApply(structs.CSIVolumeRegisterRequestType, args)
	if err != nil {
		v.logger.Error("csi raft apply failed", "error", err, "method", "register")
		return err
	}
	if respErr, ok := resp.(error); ok {
		return respErr
	}

	reply.Index = index
	v.srv.setQueryMeta(&reply.QueryMeta)
	return nil
}

// Deregister removes a set of volumes
func (v *CSIVolume) Deregister(args *structs.CSIVolumeDeregisterRequest, reply *structs.CSIVolumeDeregisterResponse) error {
	if done, err := v.srv.forward("CSIVolume.Deregister", args, args, reply); done {
		return err
	}

	aclObj, err := v.srv.WriteACLObj(&args.WriteRequest)
	if err != nil {
		return err
	}

	metricsStart := time.Now()
	defer metrics.MeasureSince([]string{"nomad", "volume", "deregister"}, metricsStart)

	ns := args.RequestNamespace()
	if !aclObj.AllowNsOp(ns, acl.NamespaceCapabilityCSICreateVolume) {
		return structs.ErrPermissionDenied
	}

	resp, index, err := v.srv.raftApply(structs.CSIVolumeDeregisterRequestType, args)
	if err != nil {
		v.logger.Error("csi raft apply failed", "error", err, "method", "deregister")
		return err
	}
	if respErr, ok := resp.(error); ok {
		return respErr
	}

	reply.Index = index
	v.srv.setQueryMeta(&reply.QueryMeta)
	return nil
}

// Claim claims a volume
func (v *CSIVolume) Claim(args *structs.CSIVolumeClaimRequest, reply *structs.CSIVolumeClaimResponse) error {
	if done, err := v.srv.forward("CSIVolume.Claim", args, args, reply); done {
		return err
	}

	aclObj, err := v.srv.WriteACLObj(&args.WriteRequest)
	if err != nil {
		return err
	}

	metricsStart := time.Now()
	defer metrics.MeasureSince([]string{"nomad", "volume", "claim"}, metricsStart)

	if !aclObj.AllowNsOp(args.RequestNamespace(), acl.NamespaceCapabilityCSIAccess) {
		return structs.ErrPermissionDenied
	}

	// we'll add the PublishContext to the reply here
	err = v.srv.controllerPublishVolume(args, reply)
	if err != nil {
		return err
	}

	resp, index, err := v.srv.raftApply(structs.CSIVolumeClaimRequestType, args)
	if err != nil {
		v.logger.Error("csi raft apply failed", "error", err, "method", "claim")
		return err
	}
	if respErr, ok := resp.(error); ok {
		return respErr
	}

	reply.Index = index
	v.srv.setQueryMeta(&reply.QueryMeta)
	return nil
}

// CSIPlugin wraps the structs.CSIPlugin with request data and server context
type CSIPlugin struct {
	srv    *Server
	logger log.Logger
}

// List replies with CSIPlugins, filtered by ACL access
func (v *CSIPlugin) List(args *structs.CSIPluginListRequest, reply *structs.CSIPluginListResponse) error {
	if done, err := v.srv.forward("CSIPlugin.List", args, args, reply); done {
		return err
	}

	aclObj, err := v.srv.QueryACLObj(&args.QueryOptions)
	if err != nil {
		return err
	}

	if !aclObj.AllowNsOp(args.RequestNamespace(), acl.NamespaceCapabilityCSIAccess) {
		return structs.ErrPermissionDenied
	}

	metricsStart := time.Now()
	defer metrics.MeasureSince([]string{"nomad", "plugin", "list"}, metricsStart)

	opts := blockingOptions{
		queryOpts: &args.QueryOptions,
		queryMeta: &reply.QueryMeta,
		run: func(ws memdb.WatchSet, state *state.StateStore) error {
			// Query all plugins
			iter, err := state.CSIPlugins(ws)
			if err != nil {
				return err
			}

			// Collect results
			var ps []*structs.CSIPluginListStub
			for {
				raw := iter.Next()
				if raw == nil {
					break
				}

				plug := raw.(*structs.CSIPlugin)

				// FIXME we should filter the ACL access for the plugin's
				// namespace, but plugins don't currently have namespaces
				ps = append(ps, plug.Stub())
			}

			reply.Plugins = ps
			return v.srv.replySetIndex(csiPluginTable, &reply.QueryMeta)
		}}
	return v.srv.blockingRPC(&opts)
}

// Get fetches detailed information about a specific plugin
func (v *CSIPlugin) Get(args *structs.CSIPluginGetRequest, reply *structs.CSIPluginGetResponse) error {
	if done, err := v.srv.forward("CSIPlugin.Get", args, args, reply); done {
		return err
	}

	aclObj, err := v.srv.QueryACLObj(&args.QueryOptions)
	if err != nil {
		return err
	}

	if !aclObj.AllowNsOp(args.RequestNamespace(), acl.NamespaceCapabilityCSIAccess) {
		return structs.ErrPermissionDenied
	}

	metricsStart := time.Now()
	defer metrics.MeasureSince([]string{"nomad", "plugin", "get"}, metricsStart)

	opts := blockingOptions{
		queryOpts: &args.QueryOptions,
		queryMeta: &reply.QueryMeta,
		run: func(ws memdb.WatchSet, state *state.StateStore) error {
			plug, err := state.CSIPluginByID(ws, args.ID)
			if err != nil {
				return err
			}

			if plug != nil {
				plug, err = state.CSIPluginDenormalize(ws, plug)
			}
			if err != nil {
				return err
			}

			// FIXME we should re-check the ACL access for the plugin's
			// namespace, but plugins don't currently have namespaces

			reply.Plugin = plug
			return v.srv.replySetIndex(csiPluginTable, &reply.QueryMeta)
		}}
	return v.srv.blockingRPC(&opts)
}

// controllerPublishVolume sends publish request to the CSI controller
// plugin associated with a volume, if any.
func (srv *Server) controllerPublishVolume(req *structs.CSIVolumeClaimRequest, resp *structs.CSIVolumeClaimResponse) error {
	plug, vol, err := srv.volAndPluginLookup(req.VolumeID)
	if err != nil {
		return err
	}
	if plug == nil || vol == nil {
		return nil // no controller for RPC
	}

	method := "ClientCSI.AttachVolume"
	cReq := &cstructs.ClientCSIControllerAttachVolumeRequest{
		PluginName:     plug.ID,
		VolumeID:       req.VolumeID,
		NodeID:         req.Allocation.NodeID,
		AttachmentMode: vol.AttachmentMode,
		AccessMode:     vol.AccessMode,
		ReadOnly:       req.Claim == structs.CSIVolumeClaimRead,
		// TODO(tgross): we don't have a way of setting these yet.
		// ref https://github.com/hashicorp/nomad/issues/7007
		// MountOptions:   vol.MountOptions,
	}
	cResp := &cstructs.ClientCSIControllerAttachVolumeResponse{}

	// TODO(tgross): add a context/cancel mechanism to client RPC.
	// This can block for arbitrarily long times, but we need to
	// make sure it completes before we can safely mark the volume as
	// claimed and return to the client so it can do a `NodePublish`.
	err = srv.csiControllerPluginRPC(plug, method, cReq, cResp)
	if err != nil {
		return err
	}
	resp.PublishContext = cResp.PublishContext
	return nil
}

// controllerUnpublishVolume sends an unpublish request to the CSI
// controller plugin associated with a volume, if any.
func (srv *Server) controllerUnpublishVolume(req *structs.CSIVolumeClaimRequest, nodeID string) error {
	plug, vol, err := srv.volAndPluginLookup(req.VolumeID)
	if err != nil {
		return err
	}
	if plug == nil || vol == nil {
		return nil // no controller for RPC
	}

	method := "ClientCSI.DetachVolume"
	cReq := &cstructs.ClientCSIControllerDetachVolumeRequest{
		PluginName: plug.ID,
		VolumeID:   req.VolumeID,
		NodeID:     nodeID,
	}
	err = srv.csiControllerPluginRPC(plug, method, cReq,
		&cstructs.ClientCSIControllerDetachVolumeResponse{})
	if err != nil {
		return err
	}
	return nil
}

func (srv *Server) volAndPluginLookup(volID string) (*structs.CSIPlugin, *structs.CSIVolume, error) {
	state := srv.fsm.State()
	ws := memdb.NewWatchSet()

	vol, err := state.CSIVolumeByID(ws, volID)
	if err != nil {
		return nil, nil, err
	}
	if vol == nil {
		return nil, nil, fmt.Errorf("volume not found: %s", volID)
	}
	if !vol.ControllerRequired {
		return nil, nil, nil // no controller to send unpublish to
	}
	plug, err := state.CSIPluginByID(ws, vol.PluginID)
	if err != nil {
		return nil, nil, err
	}
	if plug == nil {
		return nil, nil, fmt.Errorf("plugin not found: %s", vol.PluginID)
	}
	return plug, vol, nil
}

func (srv *Server) csiControllerPluginRPC(plugin *structs.CSIPlugin, method string, args, reply interface{}) error {
	// TODO(tgross): is the plugin ID unique across regions/DCs?
	// if not, can we filter the plugin by region/DC here or do we
	// need to push that down to node selection in srv.csiControllerRPC?
	for _, controller := range plugin.Controllers {
		err := srv.csiControllerRPC(controller, method, args, reply)
		if err != nil {
			return err
		}
	}
	return nil
}

func (srv *Server) csiControllerRPC(controller *structs.CSIInfo, method string, args, reply interface{}) error {
	nodeInfo := controller.NodeInfo
	if nodeInfo == nil {
		return fmt.Errorf("no node info for that controller")
	}
	err := findNodeConnAndForward(srv, nodeInfo.ID, method, args, reply)
	if err != nil {
		return err
	}
	if replyErr, ok := reply.(error); ok {
		return replyErr
	}
	return nil
}
