package mux

// OpMsg asks the multiplexer to do something. Op names are the constants below.
type OpMsg struct {
	Op  string
	Arg string
}

// Op names.
const (
	OpSplitH        = "split-h"
	OpSplitV        = "split-v"
	OpKillPane      = "kill-pane"
	OpZoom          = "zoom"
	OpFocus         = "focus"  // Arg: left|right|up|down|next|last
	OpResize        = "resize" // Arg: left|right|up|down
	OpSwap          = "swap"   // Arg: prev|next
	OpLayoutNext    = "layout-next"
	OpNewWindow     = "new-window"
	OpKillWindow    = "kill-window"
	OpSelectWindow  = "select-window" // Arg: index
	OpNextWindow    = "next-window"
	OpPrevWindow    = "prev-window"
	OpWindowList    = "window-list"
	OpRenameWindow  = "rename-window"
	OpRenameSession = "rename-session"
	OpSessionList   = "session-list"
	OpSSH           = "ssh"
	OpCopyMode      = "copy-mode"
	OpSync          = "sync"
	OpCommand       = "command"
	OpDetach        = "detach"
	OpHelp          = "help"
	OpSendPrefix    = "send-prefix"
)
