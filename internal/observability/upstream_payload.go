package observability

import (
	"encoding/base64"
	"encoding/json"
	"unicode/utf8"

	sdkusage "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/usage"
	"go.opentelemetry.io/otel/attribute"
)

func upstreamPayloadAttributes(snapshot sdkusage.UpstreamSnapshot) []attribute.KeyValue {
	if len(snapshot.HTTP) == 0 && len(snapshot.WebSocket) == 0 {
		return nil
	}
	attrs := []attribute.KeyValue{attribute.Bool("cpa.upstream.capture_complete", snapshot.Complete)}
	if len(snapshot.HTTP) == 1 && len(snapshot.WebSocket) == 0 {
		call := snapshot.HTTP[0]
		attrs = append(attrs,
			attribute.String("langfuse.observation.input", payloadText(call.Request)),
			attribute.String("langfuse.observation.output", payloadText(call.Response)),
			attribute.String("cpa.upstream.http.method", call.Method),
			attribute.Int("cpa.upstream.http.status", call.Status),
			attribute.Bool("cpa.upstream.http.request_complete", call.RequestComplete),
			attribute.Bool("cpa.upstream.http.response_complete", call.ResponseComplete),
		)
		return attrs
	}

	if len(snapshot.HTTP) > 0 {
		requests := make([]map[string]any, 0, len(snapshot.HTTP))
		responses := make([]map[string]any, 0, len(snapshot.HTTP))
		for _, call := range snapshot.HTTP {
			requests = append(requests, map[string]any{
				"method": call.Method, "body": payloadValue(call.Request), "complete": call.RequestComplete,
			})
			responses = append(responses, map[string]any{
				"status": call.Status, "body": payloadValue(call.Response), "complete": call.ResponseComplete,
			})
		}
		attrs = append(attrs,
			attribute.String("langfuse.observation.input", jsonText(requests)),
			attribute.String("langfuse.observation.output", jsonText(responses)),
		)
	}
	if len(snapshot.WebSocket) > 0 {
		timeline := make([]map[string]any, 0, len(snapshot.WebSocket))
		inputs := make([]map[string]any, 0)
		outputs := make([]map[string]any, 0)
		for _, frame := range snapshot.WebSocket {
			message := map[string]any{"opcode": frame.Opcode, "payload": payloadValue(frame.Payload)}
			timeline = append(timeline, map[string]any{
				"direction": frame.Direction, "opcode": frame.Opcode, "payload": payloadValue(frame.Payload),
			})
			if frame.Direction == "sent" {
				inputs = append(inputs, message)
			} else {
				outputs = append(outputs, message)
			}
		}
		if len(snapshot.HTTP) == 0 {
			attrs = append(attrs,
				attribute.String("langfuse.observation.input", jsonText(inputs)),
				attribute.String("langfuse.observation.output", jsonText(outputs)),
			)
		}
		attrs = append(attrs, attribute.String("cpa.upstream.websocket.timeline", jsonText(timeline)))
	}
	return attrs
}

func payloadText(raw []byte) string {
	if utf8.Valid(raw) {
		return string(raw)
	}
	return jsonText(payloadValue(raw))
}

func payloadValue(raw []byte) any {
	if utf8.Valid(raw) {
		return string(raw)
	}
	return map[string]string{"encoding": "base64", "data": base64.StdEncoding.EncodeToString(raw)}
}

func jsonText(value any) string {
	encoded, _ := json.Marshal(value)
	return string(encoded)
}
