package mobile

import _ "embed"

// RelayPy is the transparent relay script served by the hub at
// GET /api/mobile/relay.py. It is embedded so the served relay always
// matches the hub's version. The relay holds no token and never decrypts;
// it only forwards the app's /api/mobile/* requests to the hub.
//
//go:embed relay.py
var RelayPy []byte
