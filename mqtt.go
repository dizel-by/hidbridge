package main

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	mqtt "github.com/eclipse/paho.mqtt.golang"
)

// mqttBridge connects the daemon to an MQTT broker so Home Assistant can toggle
// the KVM grab and watch its state. It is an *additional* control surface — the
// local toggleKey (KEY_CALC) keeps working unchanged; both end up in applyGrab.
//
// Topic layout (base = <baseTopic>/<nodeID>, e.g. hidbridge/desktop):
//
//	<base>/status      availability, retained: "online" / "offline" (LWT)
//	<base>/grab/set    command in:  "ON" | "OFF" | "TOGGLE" (case-insensitive)
//	<base>/grab/state  state  out:  "ON" / "OFF", retained
//	<base>/link/state  bridge link up?:  "online" / "offline", retained
//
// On connect it also publishes Home Assistant MQTT-discovery configs so the
// device shows up automatically: a switch (grab) and a connectivity sensor
// (link). State is always published back, so HA stays correct even when the
// grab is flipped by the local hotkey.
type mqttBridge struct {
	cli  mqtt.Client
	cfg  MQTTConfig
	base string
	k    *kvm
	sink Sink
}

const (
	mqttOnline  = "online"
	mqttOffline = "offline"
)

func (b *mqttBridge) topic(suffix string) string { return b.base + "/" + suffix }

// startMQTT dials the broker and wires up everything. It returns once the client
// is created; the actual connection happens in the background (auto-reconnect),
// so a broker that is briefly down does not block daemon startup.
func startMQTT(cfg MQTTConfig, k *kvm, sink Sink) (*mqttBridge, error) {
	cfg = cfg.withDefaults()
	b := &mqttBridge{
		cfg:  cfg,
		base: cfg.BaseTopic + "/" + cfg.NodeID,
		k:    k,
		sink: sink,
	}

	opts := mqtt.NewClientOptions().
		AddBroker(cfg.Broker).
		SetClientID(cfg.ClientID).
		SetUsername(cfg.User).
		SetPassword(cfg.Password).
		SetCleanSession(true).
		SetAutoReconnect(true).
		SetConnectRetry(true).
		SetConnectRetryInterval(5 * time.Second).
		SetConnectTimeout(10 * time.Second).
		SetKeepAlive(30 * time.Second).
		// Last will: if the daemon dies, the broker marks us offline so HA
		// shows the device unavailable instead of stuck on a stale state.
		SetWill(b.topic("status"), mqttOffline, 1, true).
		SetOnConnectHandler(b.onConnect).
		SetConnectionLostHandler(func(_ mqtt.Client, err error) {
			logErr("mqtt: connection lost: %v", err)
		})

	b.cli = mqtt.NewClient(opts)
	if tok := b.cli.Connect(); tok.WaitTimeout(10*time.Second) && tok.Error() != nil {
		// Connect() with retry keeps trying in the background; surface the
		// first error but don't fail the daemon over a broker that's down.
		logErr("mqtt: initial connect to %s: %v (will keep retrying)", cfg.Broker, tok.Error())
	}

	// Push every grab transition to the broker — including ones caused by the
	// local hotkey or a forced release — so HA always reflects reality.
	k.notify = func(grabbed bool) { go b.publishGrab(grabbed) }

	go b.watchLink()
	return b, nil
}

// onConnect (re)establishes all retained state after each connect/reconnect:
// discovery configs, availability, current grab + link state, and the command
// subscription. Doing it here (not once at startup) means a broker restart
// re-seeds discovery without restarting the daemon.
func (b *mqttBridge) onConnect(cli mqtt.Client) {
	b.publishDiscovery()
	b.pub(b.topic("status"), mqttOnline, true)
	b.publishGrab(b.k.isGrabbed())
	b.publishLink(b.sink.Alive())
	if tok := cli.Subscribe(b.topic("grab/set"), 1, b.onGrabSet); tok.Wait() && tok.Error() != nil {
		logErr("mqtt: subscribe grab/set: %v", tok.Error())
	}
	fmt.Printf("mqtt: connected to %s as %q (base topic %s)\n", b.cfg.Broker, b.cfg.ClientID, b.base)
}

// onGrabSet handles an inbound command from Home Assistant.
func (b *mqttBridge) onGrabSet(_ mqtt.Client, msg mqtt.Message) {
	cmd := strings.TrimSpace(string(msg.Payload()))
	switch strings.ToLower(cmd) {
	case "toggle":
		b.k.ToggleGrab()
	default:
		on, ok := onOff(cmd)
		if !ok {
			logErr("mqtt: ignoring grab/set payload %q (want ON|OFF|TOGGLE)", cmd)
			return
		}
		b.k.SetGrab(on)
	}
	// State is published by the notify hook; no need to echo here.
}

func (b *mqttBridge) publishGrab(grabbed bool) {
	b.pub(b.topic("grab/state"), onOffStr(grabbed), true)
}

func (b *mqttBridge) publishLink(alive bool) {
	b.pub(b.topic("link/state"), onlineStr(alive), true)
}

// watchLink mirrors the bridge link health (USB node present / BLE or TCP up)
// into HA as a connectivity sensor, publishing only on change.
func (b *mqttBridge) watchLink() {
	last := b.sink.Alive()
	b.publishLink(last)
	t := time.NewTicker(2 * time.Second)
	defer t.Stop()
	for range t.C {
		if now := b.sink.Alive(); now != last {
			last = now
			b.publishLink(now)
		}
	}
}

func (b *mqttBridge) pub(topic, payload string, retain bool) {
	if b.cli == nil {
		return
	}
	b.cli.Publish(topic, 1, retain, payload) // fire-and-forget; reconnect resends retained state
}

// publishDiscovery announces the device to Home Assistant via MQTT discovery.
func (b *mqttBridge) publishDiscovery() {
	dev := map[string]any{
		"identifiers":  []string{"hidbridge_" + b.cfg.NodeID},
		"name":         b.cfg.DeviceName,
		"manufacturer": "hidbridge",
		"model":        "Linux→ESP32-S3 USB-HID KVM",
	}
	avail := b.topic("status")

	grab := map[string]any{
		"name":                "KVM grab",
		"unique_id":           b.cfg.NodeID + "_grab",
		"command_topic":       b.topic("grab/set"),
		"state_topic":         b.topic("grab/state"),
		"payload_on":          "ON",
		"payload_off":         "OFF",
		"availability_topic":  avail,
		"payload_available":   mqttOnline,
		"payload_unavailable": mqttOffline,
		"icon":                "mdi:keyboard-settings",
		"device":              dev,
	}
	link := map[string]any{
		"name":                "Bridge link",
		"unique_id":           b.cfg.NodeID + "_link",
		"state_topic":         b.topic("link/state"),
		"payload_on":          mqttOnline,
		"payload_off":         mqttOffline,
		"device_class":        "connectivity",
		"availability_topic":  avail,
		"payload_available":   mqttOnline,
		"payload_unavailable": mqttOffline,
		"device":              dev,
	}

	b.pubJSON(b.discoveryTopic("switch", "grab"), grab)
	b.pubJSON(b.discoveryTopic("binary_sensor", "link"), link)
}

func (b *mqttBridge) discoveryTopic(component, object string) string {
	return fmt.Sprintf("%s/%s/%s/%s/config", b.cfg.DiscoveryPrefix, component, b.cfg.NodeID, object)
}

func (b *mqttBridge) pubJSON(topic string, v any) {
	data, err := json.Marshal(v)
	if err != nil {
		logErr("mqtt: marshal discovery %s: %v", topic, err)
		return
	}
	b.pub(topic, string(data), true)
}

// Close marks the daemon offline and disconnects cleanly. Safe to call on a nil
// bridge (when MQTT is disabled).
func (b *mqttBridge) Close() {
	if b == nil || b.cli == nil {
		return
	}
	tok := b.cli.Publish(b.topic("status"), 1, true, mqttOffline)
	tok.WaitTimeout(500 * time.Millisecond)
	b.cli.Disconnect(250)
}

func onOffStr(on bool) string {
	if on {
		return "ON"
	}
	return "OFF"
}

func onlineStr(up bool) string {
	if up {
		return mqttOnline
	}
	return mqttOffline
}
