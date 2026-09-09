//go:build windows

package tray

import _ "embed"

// iconData is the tray icon, a multi-resolution ICO (16/20/24/32/48 px).
// Parsed at runtime by iconFromICO; a failure falls back to the stock
// application icon so the tray always appears.
//
//go:embed icon.ico
var iconData []byte
