package wire

import "encoding/json"

// ClientJS renders the protocol as a script that sets window.AW.
func ClientJS() []byte {
	aw := struct {
		Subprotocol  string                 `json:"subprotocol"`
		BearerPrefix string                 `json:"bearerPrefix"`
		Kind         map[string]Kind        `json:"kind"`
		Payload      map[string]payloadForm `json:"payload"`
		Close        map[string]CloseCode   `json:"close"`
		MaxInput     int                    `json:"maxInput"`
	}{
		Subprotocol:  Subprotocol,
		BearerPrefix: BearerPrefix,
		Kind:         map[string]Kind{},
		Payload:      map[string]payloadForm{},
		Close:        closeNames,
		MaxInput:     MaxInput,
	}
	for k, spec := range kinds {
		aw.Kind[spec.name] = k
		aw.Payload[spec.name] = spec.payload
	}
	b, err := json.MarshalIndent(aw, "", "  ")
	if err != nil {
		panic("wire: ClientJS: " + err.Error())
	}
	return append(append([]byte("window.AW = "), b...), ";\n"...)
}
