-- Prosody configuration for the mautrix-xmpp integration tests. Not for
-- production use: accounts and certificates are created by entrypoint.sh.

admins = {}
plugin_paths = {}

modules_enabled = {
	"disco"; "roster"; "saslauth"; "tls"; "ping"; "version"; "time";
	"carbons";      -- XEP-0280
	"smacks";       -- XEP-0198
	"mam";          -- XEP-0313
	"pep";          -- XEP-0163, needed for XEP-0402 bookmarks
	"private";      -- XEP-0049, needed for XEP-0048 bookmarks
	"bookmarks";    -- XEP-0402 <-> XEP-0048 sync
	"vcard_legacy"; "vcard4"; -- XEP-0054
	"blocklist";
}
modules_disabled = { "s2s" }

authentication = "internal_hashed"
storage = "internal"
data_path = "/var/lib/prosody"
pidfile = "/var/run/prosody/prosody.pid"

interfaces = { "*" }
c2s_require_encryption = true
c2s_direct_tls_ports = { 5223 }
allow_registration = false
-- Keep the test server quiet and fast.
limits = {}
archive_expires_after = "never"
default_archive_policy = true
-- Short hibernation so reconnect tests exercise the "session gone" path.
smacks_hibernation_time = 5

log = { { levels = { min = "info" }, to = "console" } }

certificates = "/etc/prosody/certs"
https_certificate = "/etc/prosody/certs/xmpp.test.crt"
https_key = "/etc/prosody/certs/xmpp.test.key"
http_ports = {}
https_ports = { 5281 }
https_interfaces = { "*" }

VirtualHost "xmpp.test"

Component "conference.xmpp.test" "muc"
	modules_enabled = { "muc_mam" }
	restrict_room_creation = false
	muc_room_locking = false
	muc_room_default_persistent = true
	muc_room_default_public = true
	muc_room_default_public_jids = true
	muc_log_by_default = true
	muc_log_all_rooms = true

Component "upload.xmpp.test" "http_file_share"
	http_file_share_size_limit = 10 * 1024 * 1024
	http_external_url = "https://upload.xmpp.test:5281/"
