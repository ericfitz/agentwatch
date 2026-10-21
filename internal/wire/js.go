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
		MaxChunk     int                    `json:"maxChunk"`
		MaxInput     int                    `json:"maxInput"`
		UploadWindow int                    `json:"uploadWindow"`
		ChunkHeader  int                    `json:"chunkHeader"`
		ChunkIDBytes int                    `json:"chunkIdBytes"`
		FailText     map[FailReason]string  `json:"failText"`
	}{
		Subprotocol:  Subprotocol,
		BearerPrefix: BearerPrefix,
		Kind:         map[string]Kind{},
		Payload:      map[string]payloadForm{},
		Close:        closeNames,
		MaxChunk:     MaxChunk,
		MaxInput:     MaxInput,
		UploadWindow: UploadWindow,
		ChunkHeader:  chunkHeader,
		ChunkIDBytes: chunkIDBytes,
		FailText:     failText,
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
