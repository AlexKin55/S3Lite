package util

const (
	// DefaultVolumeSizeLimitMB is the s3lite cluster's volume size limit,
	// expressed in the MiB units used by volumeSizeLimitMB. 500 MB keeps EC
	// shards small (~85 MB data each) and lets the coder start soon after a
	// burst of writes; with the per-node quota handed out by the control
	// plane (STORAGE_SIZE / this value = volume slots) it also caps the raw
	// pre-EC buffer of a node.
	// This is deliberately NOT configurable at runtime: changing it requires
	// recompiling weed (and reviewing the agents' STORAGE_SIZE quotas), so
	// the change is always a conscious one. The flag -volumeSizeLimitMB can
	// still override it for experiments.
	DefaultVolumeSizeLimitMB = 500
	// MaxVolumeSizeLimitMB expresses VolumeSizeLimitGB in MiB: 30 GiB for
	// 4-byte offsets and 8,000 GiB for 5-byte offsets.
	MaxVolumeSizeLimitMB = VolumeSizeLimitGB * KiByte
)
