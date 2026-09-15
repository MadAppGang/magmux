package httpapi

import (
	"encoding/json"
	"net/http"

	"github.com/MadAppGang/magmux/protocol"
)

// StatusFor maps one of magmux's protocol codes onto an HTTP status.
//
// The map lives HERE and nowhere else. The hub speaks magmux's vocabulary and
// has no opinion about HTTP; the socket has no statuses at all; and a code that
// grew a second mapping in a second adapter would mean the same failure
// answered 409 on one transport and 400 on another.
//
// The choices worth defending:
//
//   - 404 for unknown_verb, not 501. The op is addressed as a resource
//     (POST /v1/ops/{name}), and "there is no such op" is a missing resource.
//     unsupported is 501, because that one means magmux has no such FEATURE.
//   - 409 for the whole family of "the pane is real but not in a state that can
//     serve this" — dead, hidden, the control panel, no controller, no
//     transcript. They are conflicts with current state, not bad requests: the
//     same bytes would have worked a moment ago or will work later.
//   - 502 for plugin_gone. magmux is the gateway and the plugin is the upstream.
//   - 503 for not_ready, with the layout still being built, which is exactly
//     what 503 means. Its companion, busy, is 429: that one is about RATE, and
//     a client should slow down rather than wait for a different state.
func StatusFor(code string) int {
	switch code {
	case "":
		return http.StatusOK
	case protocol.CodeBadRequest, protocol.CodeTooSmall:
		return http.StatusBadRequest
	case protocol.CodeUnauthorized:
		return http.StatusUnauthorized
	case protocol.CodeForbidden:
		return http.StatusForbidden
	case protocol.CodeNoSuchPane, protocol.CodeUnknownVerb:
		return http.StatusNotFound
	case protocol.CodePaneDead, protocol.CodePaneIsControl, protocol.CodePaneHidden,
		protocol.CodeNoController, protocol.CodeNoTranscript:
		return http.StatusConflict
	case protocol.CodeTooLarge:
		return http.StatusRequestEntityTooLarge
	case protocol.CodeBusy:
		return http.StatusTooManyRequests
	case protocol.CodeUnsupported:
		return http.StatusNotImplemented
	case protocol.CodePluginGone:
		return http.StatusBadGateway
	case protocol.CodeNotReady:
		return http.StatusServiceUnavailable
	case protocol.CodeTimeout:
		return http.StatusGatewayTimeout
	}
	return http.StatusInternalServerError
}

// writeJSON writes one object with the given status.
func writeJSON(w http.ResponseWriter, status int, v any) {
	body, err := json.Marshal(v)
	if err != nil {
		// Unreachable for the shapes this package builds, and a 500 with a
		// fixed body is the only honest answer if it ever is reached.
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"ok":false,"code":"internal","error":"the reply could not be encoded"}`))
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(status)
	_, _ = w.Write(append(body, '\n'))
}

// writeErrStatus answers with magmux's failure shape at an explicit status,
// for the one case where the code's usual status is the wrong answer.
//
// That case is the connection cap. `busy` is 429 everywhere else, because
// everywhere else it means a RATE the caller should back off from — a full lane,
// a Sub with sixteen ops in flight. At the listener it means CAPACITY, the
// resource is unavailable and the caller has done nothing wrong, which is 503
// with Retry-After.
func writeErrStatus(w http.ResponseWriter, status int, err error) {
	writeJSON(w, status, map[string]any{
		"ok": false, "code": protocol.CodeOf(err), "error": err.Error(),
	})
}

// writeErr answers with magmux's failure shape, at the status its code maps to.
//
// The body always carries BOTH the code and the message: the status is a coarse
// class that several codes share (409 covers five of them), and a client that
// branched on the status alone could not tell "the pane is dead" from "the pane
// is the control panel".
func writeErr(w http.ResponseWriter, err error) {
	code := protocol.CodeOf(err)
	writeJSON(w, StatusFor(code), map[string]any{
		"ok": false, "code": code, "error": err.Error(),
	})
}
