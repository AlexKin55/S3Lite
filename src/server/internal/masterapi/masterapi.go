package masterapi

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"time"

	"github.com/seaweedfs/seaweedfs/weed/pb/master_pb"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

// Client talks to the SeaweedFS master over gRPC and exposes the cluster
// topology (regular volumes + EC shards with their per-volume ratio).
type Client struct {
	httpAddr string // e.g. "192.168.1.9:9333"
	grpcAddr string // e.g. "192.168.1.9:19333" (http port + 10000)
	timeout  time.Duration
	http     http.Client
}

func New(httpAddr string, timeout time.Duration) *Client {
	return &Client{
		httpAddr: httpAddr,
		grpcAddr: grpcPort(httpAddr),
		timeout:  timeout,
	}
}

// CollectionUsage summarizes the storage footprint of one collection,
// aggregated over the whole topology (regular volumes + EC shards).
type CollectionUsage struct {
	Name     string `json:"name"`
	Size     int64  `json:"size"`      // bytes (regular volumes + EC shards)
	Files    int64  `json:"files"`     // file count reported by the volume servers
	Volumes  int    `json:"volumes"`   // regular (full) volume count
	EcShards int    `json:"ec_shards"` // individual EC shard count
}

// CollectionUsage fetches the topology and aggregates per-collection usage.
func (c *Client) CollectionUsage(ctx context.Context) ([]CollectionUsage, error) {
	topo, _, err := c.Topology(ctx)
	if err != nil {
		return nil, err
	}
	agg := map[string]*CollectionUsage{}
	get := func(name string) *CollectionUsage {
		u, ok := agg[name]
		if !ok {
			u = &CollectionUsage{Name: name}
			agg[name] = u
		}
		return u
	}
	for _, dci := range topo.GetDataCenterInfos() {
		for _, rack := range dci.GetRackInfos() {
			for _, dn := range rack.GetDataNodeInfos() {
				for _, di := range dn.GetDiskInfos() {
					for _, v := range di.GetVolumeInfos() {
						u := get(v.GetCollection())
						u.Size += int64(v.GetSize())
						u.Files += int64(v.GetFileCount())
						u.Volumes++
					}
					for _, e := range di.GetEcShardInfos() {
						u := get(e.GetCollection())
						for _, s := range e.GetShardSizes() {
							u.Size += s
						}
						u.Files += int64(e.GetFileCount())
						u.EcShards++
					}
				}
			}
		}
	}
	out := make([]CollectionUsage, 0, len(agg))
	for _, u := range agg {
		out = append(out, *u)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// DeleteCollection removes a collection from the cluster: the master deletes
// the collection's regular volumes AND its EC shards on every volume server.
// This is the destructive backend of the dashboard's "clean orphaned data"
// action. It talks to the master over its HTTP admin API (/col/delete), which
// transparently proxies to the leader.
func (c *Client) DeleteCollection(ctx context.Context, name string) error {
	target := "http://" + c.httpAddr + "/col/delete?collection=" + url.QueryEscape(name)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, target, nil)
	if err != nil {
		return fmt.Errorf("build /col/delete request: %w", err)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("POST %s: %w", target, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("master /col/delete %q: HTTP %d: %s", name, resp.StatusCode, string(body))
	}
	return nil
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
