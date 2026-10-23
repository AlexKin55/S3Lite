module s3lite.dev/server

go 1.27.0

require (
	github.com/seaweedfs/seaweedfs/weed/pb/master_pb v0.0.0
	google.golang.org/grpc v1.84.0
)

require (
	github.com/seaweedfs/seaweedfs/weed/pb/remote_pb v0.0.0 // indirect
	github.com/seaweedfs/seaweedfs/weed/pb/volume_server_pb v0.0.0 // indirect
	golang.org/x/net v0.57.0 // indirect
	golang.org/x/sys v0.47.0 // indirect
	golang.org/x/text v0.40.0 // indirect
	google.golang.org/genproto/googleapis/rpc v0.0.0-20260706201446-f0a921348800 // indirect
	google.golang.org/protobuf v1.36.11 // indirect
)

replace github.com/seaweedfs/seaweedfs/weed/pb/master_pb => ./gen/weed/pb/master_pb

replace github.com/seaweedfs/seaweedfs/weed/pb/volume_server_pb => ./gen/weed/pb/volume_server_pb

replace github.com/seaweedfs/seaweedfs/weed/pb/remote_pb => ./gen/weed/pb/remote_pb
