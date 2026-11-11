package protocol

// The MCP binding of WATCH (SPEC §6.8.1): the _meta keys a gateway writes on
// a subscription's notifications and a client sends on its listen request.
const (
	WatchMetaSince        = "demarkus.io/since"
	WatchMetaCoalesce     = "demarkus.io/coalesce"
	WatchMetaSubscription = "demarkus.io/subscription"
	WatchMetaCursor       = "demarkus.io/cursor"
	WatchMetaPath         = "demarkus.io/path"
	WatchMetaVersion      = "demarkus.io/version"
	WatchMetaOp           = "demarkus.io/op"
	WatchMetaHash         = "demarkus.io/hash"
	WatchMetaAgent        = "demarkus.io/agent"
	WatchMetaUser         = "demarkus.io/user"
	WatchMetaResync       = "demarkus.io/resync"
	WatchMetaStatus       = "demarkus.io/status"
)

// WatchSubscriptionURI is how both ends name a subscription to path in world:
// the world name lowercase, the path as the URI parser decoded it.
func WatchSubscriptionURI(world, path string) string { return ALPN + "://" + world + path }
