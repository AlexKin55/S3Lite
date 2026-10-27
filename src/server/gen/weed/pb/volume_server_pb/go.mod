module github.com/seaweedfs/seaweedfs/weed/pb/volume_server_pb

go 1.26.0

require (
	github.com/seaweedfs/seaweedfs/weed/pb/remote_pb v0.0.0
	google.golang.org/grpc v1.84.0
	google.golang.org/protobuf v1.36.11
)

replace github.com/seaweedfs/seaweedfs/weed/pb/remote_pb => ../remote_pb
