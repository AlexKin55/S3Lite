package masterapi

import (
	"context"
	"fmt"
	"time"

	"github.com/seaweedfs/seaweedfs/weed/pb/master_pb"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

// Client talks to the SeaweedFS master over gRPC and exposes the cluster
// topology (regular volumes + EC shards with their per-volume ratio).
type Client struct {
	grpcAddr string // e.g. "192.168.1.9:19333" (http port + 10000)
	timeout  time.Duration
}

func New(httpAddr string, timeout time.Duration) *Client {
	return &Client{grpcAddr: grpcPort(httpAddr), timeout: timeout}
}

// grpcPort derives the master gRPC port from its HTTP address (http + 10000).
func grpcPort(httpAddr string) string {
	for i := len(httpAddr) - 1; i >= 0; i-- {
		if httpAddr[i] == ':' {
			return httpAddr[:i] + ":" + portPlus(httpAddr[i+1:], 10000)
		}
	}
	return httpAddr + ":19333"
}

func portPlus(p string, add int) string {
	n := 0
	for _, c := range p {
		if c < '0' || c > '9' {
			return p
		}
		n = n*10 + int(c-'0')
	}
	return itoa(n + add)
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	s := ""
	for n > 0 {
		s = string(rune('0'+n%10)) + s
		n /= 10
	}
	return s
}

// Topology fetches the full cluster topology (VolumeList RPC).
func (c *Client) Topology(ctx context.Context) (*master_pb.TopologyInfo, uint64, error) {
	ctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()

	conn, err := grpc.NewClient(c.grpcAddr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		return nil, 0, fmt.Errorf("dial master %s: %w", c.grpcAddr, err)
	}
	defer conn.Close()

	client := master_pb.NewSeaweedClient(conn)
	resp, err := client.VolumeList(ctx, &master_pb.VolumeListRequest{})
	if err != nil {
		return nil, 0, fmt.Errorf("VolumeList: %w", err)
	}
	return resp.GetTopologyInfo(), resp.GetVolumeSizeLimitMb(), nil
}
