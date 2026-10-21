// Package proto holds the device-plane contract and its generated Go bindings.
//
// The tests here are contract tests, not tests of protobuf itself. They pin the
// properties the wire format has to keep as the schema evolves, because both
// ends of this contract are deployed independently: a server rolls forward
// while thousands of Macs are still running an older agent.
package proto

import (
	"testing"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/known/timestamppb"

	v1 "github.com/visionify/edgepilot/packages/proto/gen/edgepilot/v1"
)

func TestAgentMessageRoundTrips(t *testing.T) {
	t.Parallel()

	original := &v1.AgentMessage{
		Seq:         42,
		SentAt:      timestamppb.New(timestamppb.Now().AsTime()),
		Traceparent: "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01",
		Payload: &v1.AgentMessage_Metrics{
			Metrics: &v1.MetricsSample{
				CpuPercent:      12.5,
				MemoryUsedBytes: 8 << 30,
				UptimeSeconds:   864000,
				TopProcesses: []*v1.ProcessSample{
					{Pid: 1, Name: "launchd", CpuPercent: 0.1, MemoryBytes: 12 << 20},
				},
			},
		},
	}

	wire, err := proto.Marshal(original)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	var decoded v1.AgentMessage
	if err := proto.Unmarshal(wire, &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !proto.Equal(original, &decoded) {
		t.Fatalf("round trip changed the message:\n got %v\nwant %v", &decoded, original)
	}

	metrics, ok := decoded.GetPayload().(*v1.AgentMessage_Metrics)
	if !ok {
		t.Fatalf("payload type = %T, want metrics", decoded.GetPayload())
	}
	if got := metrics.Metrics.GetCpuPercent(); got != 12.5 {
		t.Errorf("cpu_percent = %v, want 12.5", got)
	}
}

// An absent temperature reading must stay absent. Apple silicon frequently
// exposes no readable SMC sensor, and a device reporting a steady 0 °C would be
// indistinguishable from a genuinely cold one — which is exactly the confusion
// the optional field exists to prevent.
func TestAbsentTemperatureIsDistinguishableFromZero(t *testing.T) {
	t.Parallel()

	absent := &v1.MetricsSample{CpuPercent: 10}
	zero := &v1.MetricsSample{CpuPercent: 10, TemperatureC: proto.Float32(0)}

	for name, sample := range map[string]*v1.MetricsSample{"absent": absent, "zero": zero} {
		wire, err := proto.Marshal(sample)
		if err != nil {
			t.Fatalf("%s: marshal: %v", name, err)
		}
		var decoded v1.MetricsSample
		if err := proto.Unmarshal(wire, &decoded); err != nil {
			t.Fatalf("%s: unmarshal: %v", name, err)
		}
		wantPresent := name == "zero"
		if got := decoded.TemperatureC != nil; got != wantPresent {
			t.Errorf("%s: presence = %v, want %v", name, got, wantPresent)
		}
	}
}

// A peer must be able to receive a message type it does not know about without
// dropping the stream. Unknown fields survive an unmarshal/marshal cycle, which
// is what lets an old agent tolerate a new server.
func TestUnknownFieldsSurviveRoundTrip(t *testing.T) {
	t.Parallel()

	// Field 999 does not exist in ServerMessage: varint field 999 = tag
	// (999<<3 | 0), value 1.
	future := []byte{0xB8, 0x3E, 0x01}
	msg := &v1.ServerMessage{Seq: 7}
	wire, err := proto.Marshal(msg)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	wire = append(wire, future...)

	var decoded v1.ServerMessage
	if err := proto.Unmarshal(wire, &decoded); err != nil {
		t.Fatalf("an unknown field must not fail the unmarshal: %v", err)
	}
	if decoded.GetSeq() != 7 {
		t.Errorf("known field lost: seq = %d", decoded.GetSeq())
	}
	if len(decoded.ProtoReflect().GetUnknown()) == 0 {
		t.Error("unknown field was dropped; forward compatibility depends on retaining it")
	}
}

// Every enum needs a zero-valued *_UNSPECIFIED member, so an unset field is
// distinguishable from a deliberate first value. Getting this wrong makes a
// missing field silently mean something.
func TestEnumsReserveZeroForUnspecified(t *testing.T) {
	t.Parallel()

	files := (&v1.ServerMessage{}).ProtoReflect().Descriptor().ParentFile()
	checked := 0

	walk := func(enums protoreflect.EnumDescriptors) {
		for i := range enums.Len() {
			e := enums.Get(i)
			zero := e.Values().ByNumber(0)
			if zero == nil {
				t.Errorf("enum %s has no zero value", e.FullName())
				continue
			}
			if got := string(zero.Name()); !hasSuffix(got, "_UNSPECIFIED") {
				t.Errorf("enum %s zero value is %q, want a *_UNSPECIFIED member", e.FullName(), got)
			}
			checked++
		}
	}

	walk(files.Enums())
	for i := range files.Messages().Len() {
		walk(files.Messages().Get(i).Enums())
	}

	if checked == 0 {
		t.Fatal("no enums were checked; the walk is not finding them")
	}
}

func hasSuffix(s, suffix string) bool {
	return len(s) >= len(suffix) && s[len(s)-len(suffix):] == suffix
}

// The oneof arms are the protocol's vocabulary. This pins that every message
// type the design calls for actually exists and is reachable, so a rename or a
// deletion fails here rather than at runtime on a customer's Mac.
func TestProtocolVocabulary(t *testing.T) {
	t.Parallel()

	// Assigning each arm into its envelope is itself the check: a renamed,
	// removed, or misplaced arm fails to compile.
	agentArms := []*v1.AgentMessage{
		{Payload: &v1.AgentMessage_Hello{}},
		{Payload: &v1.AgentMessage_Heartbeat{}},
		{Payload: &v1.AgentMessage_Metrics{}},
		{Payload: &v1.AgentMessage_Inventory{}},
		{Payload: &v1.AgentMessage_CommandProgress{}},
		{Payload: &v1.AgentMessage_CommandResult{}},
		{Payload: &v1.AgentMessage_FileChunk{}},
		{Payload: &v1.AgentMessage_FileListing{}},
		{Payload: &v1.AgentMessage_FileOperationResult{}},
		{Payload: &v1.AgentMessage_TunnelOpened{}},
		{Payload: &v1.AgentMessage_TunnelData{}},
		{Payload: &v1.AgentMessage_TunnelClosed{}},
		{Payload: &v1.AgentMessage_DeploymentProgress{}},
		{Payload: &v1.AgentMessage_LogStreamChunk{}},
		{Payload: &v1.AgentMessage_CertificateRequest{}},
		{Payload: &v1.AgentMessage_Event{}},
		{Payload: &v1.AgentMessage_Ack{}},
		{Payload: &v1.AgentMessage_Pong{}},
	}
	serverArms := []*v1.ServerMessage{
		{Payload: &v1.ServerMessage_Welcome{}},
		{Payload: &v1.ServerMessage_Config{}},
		{Payload: &v1.ServerMessage_Command{}},
		{Payload: &v1.ServerMessage_CommandCancel{}},
		{Payload: &v1.ServerMessage_FileOperation{}},
		{Payload: &v1.ServerMessage_FileChunk{}},
		{Payload: &v1.ServerMessage_TunnelOpen{}},
		{Payload: &v1.ServerMessage_TunnelData{}},
		{Payload: &v1.ServerMessage_TunnelClose{}},
		{Payload: &v1.ServerMessage_Deployment{}},
		{Payload: &v1.ServerMessage_LogStreamRequest{}},
		{Payload: &v1.ServerMessage_Certificate{}},
		{Payload: &v1.ServerMessage_Ack{}},
		{Payload: &v1.ServerMessage_Ping{}},
		{Payload: &v1.ServerMessage_Disconnect{}},
	}

	for _, m := range agentArms {
		if m.GetPayload() == nil {
			t.Errorf("an AgentMessage arm did not survive assignment: %v", m)
		}
	}
	for _, m := range serverArms {
		if m.GetPayload() == nil {
			t.Errorf("a ServerMessage arm did not survive assignment: %v", m)
		}
	}

	// Compare against the descriptor, so an arm added to the .proto but never
	// listed here is a failure rather than an untested message type.
	assertOneofCovered(t, (&v1.AgentMessage{}).ProtoReflect().Descriptor(), "payload", len(agentArms))
	assertOneofCovered(t, (&v1.ServerMessage{}).ProtoReflect().Descriptor(), "payload", len(serverArms))
}

func assertOneofCovered(t *testing.T, md protoreflect.MessageDescriptor, oneof string, covered int) {
	t.Helper()
	od := md.Oneofs().ByName(protoreflect.Name(oneof))
	if od == nil {
		t.Fatalf("%s has no oneof %q", md.FullName(), oneof)
	}
	if got := od.Fields().Len(); got != covered {
		t.Errorf("%s.%s has %d arms but the test covers %d; "+
			"a new message type was added to the protocol without a contract test",
			md.FullName(), oneof, got, covered)
	}
}
