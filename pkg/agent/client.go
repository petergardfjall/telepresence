package agent

import (
	"context"
	"time"

	"github.com/datawire/dlib/dlog"
	"github.com/golang/protobuf/ptypes/empty"
	"google.golang.org/grpc"

	"github.com/datawire/telepresence2/pkg/rpc"
)

func TalkToManager(ctx context.Context, address string, info *rpc.AgentInfo, state *State) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	conn, err := grpc.DialContext(ctx, address, grpc.WithInsecure(), grpc.WithBlock())
	if err != nil {
		return err
	}
	defer conn.Close()

	manager := rpc.NewManagerClient(conn)

	ver, err := manager.Version(ctx, &empty.Empty{})
	if err != nil {
		return err
	}

	dlog.Infof(ctx, "Connected to Manager %s", ver.Version)

	session, err := manager.ArriveAsAgent(ctx, info)
	if err != nil {
		return err
	}

	defer func() {
		if _, err := manager.Depart(ctx, session); err != nil {
			dlog.Debugf(ctx, "depart session: %+v", err)
		}
	}()

	// Call WatchIntercepts
	stream, err := manager.WatchIntercepts(ctx, session)
	if err != nil {
		return err
	}

	snapshots := make(chan *rpc.InterceptInfoSnapshot)
	go func() {
		defer cancel() // Drop the gRPC connection if we leave this function

		for {
			snapshot, err := stream.Recv()
			if err != nil {
				dlog.Debugf(ctx, "stream recv: %+v", err) // May be io.EOF
				return
			}
			snapshots <- snapshot
		}
	}()

	defer func() {
		// Reset state by processing an empty snapshot
		// - clear out any intercepts
		// - set forwarding to the app
		state.HandleIntercepts(nil)
	}()

	// Loop calling Remain
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return nil
		case snapshot := <-snapshots:
			reviews := state.HandleIntercepts(snapshot.Intercepts)
			for _, review := range reviews {
				review.Session = session
				if _, err := manager.ReviewIntercept(ctx, review); err != nil {
					return err
				}
			}
		case <-ticker.C:
		}

		if _, err := manager.Remain(ctx, session); err != nil {
			return err
		}
	}
}
