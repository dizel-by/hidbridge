// hidbridge ESP32-S3 firmware (ESP-IDF) — USB-HID bridge, cable + BLE.
//
// Native USB-OTG (TinyUSB) presents a keyboard + mouse to the TARGET computer.
// HID report frames arrive from the PC daemon over EITHER:
//   - UART0 (the onboard USB-UART bridge), and/or
//   - BLE (a Nordic-UART-style GATT characteristic, Write Without Response).
// Both feed the same parser; pick a transport just by how you connect.
//
// Topology:
//   PC --USB(UART) or BLE--> ESP32-S3 --USB-OTG(HID)--> TARGET PC
//
// BLE is used instead of Wi-Fi because a dedicated ~7.5 ms connection interval
// has far lower jitter than 2.4 GHz Wi-Fi through a router, and the laptop keeps
// its normal Wi-Fi (separate radio).
//
// Wire framing (PC -> ESP32), see sink.go:
//   0xAB  sync
//   type  1 = keyboard (8-byte boot report), 2 = mouse (6 bytes)
//   ...   payload
//   csum  XOR of type and all payload bytes
//   mouse payload: buttons, dxLo, dxHi, dyLo, dyHi, wheel  (int16 LE x/y)

#include <string.h>
#include "freertos/FreeRTOS.h"
#include "freertos/task.h"
#include "freertos/semphr.h"
#include "freertos/stream_buffer.h"
#include "esp_log.h"
#include "esp_timer.h"
#include "esp_system.h"
#include "nvs_flash.h"
#include "nvs.h"
#include "driver/uart.h"
#include "tinyusb.h"
#include "class/hid/hid_device.h"

#include "nimble/nimble_port.h"
#include "nimble/nimble_port_freertos.h"
#include "host/ble_hs.h"
#include "host/util/util.h"
#include "services/gap/ble_svc_gap.h"
#include "services/gatt/ble_svc_gatt.h"

#include "freertos/event_groups.h"
#include "esp_wifi.h"
#include "esp_event.h"
#include "esp_netif.h"
#include "mdns.h"
#include "lwip/sockets.h"

static const char *TAG = "hidbridge";

// Wi-Fi credentials come from .env (injected as compile definitions by
// main/CMakeLists.txt). If .env is absent they fall back to the Kconfig values
// (default "changeme" => Wi-Fi stays off, cable + BLE only).
#ifndef WIFI_SSID
#define WIFI_SSID CONFIG_HIDBRIDGE_WIFI_SSID
#endif
#ifndef WIFI_PASSWORD
#define WIFI_PASSWORD CONFIG_HIDBRIDGE_WIFI_PASSWORD
#endif

// Effective Wi-Fi credentials: NVS (set over serial) overrides the compile-time
// .env/Kconfig defaults, so a generic firmware can be configured without rebuild.
static char g_ssid[33];
static char g_pass[64];

// ---- UART (data from PC over the cable) ----
#define DATA_UART   UART_NUM_0
#define DATA_BAUD   921600
#define DATA_RX_BUF 2048
#define DATA_TX_PIN 43
#define DATA_RX_PIN 44

// ---- Frame types: 1/2 are HID report IDs, 3/4 are control/config commands ----
// type 4 (config) is length-prefixed (0xAB,4,len,payload[len],csum) so it can
// carry variable-length data; types 1/2/3 are fixed length.
enum { RID_KEYBOARD = 1, RID_MOUSE = 2, TYPE_CONTROL = 3, TYPE_CONFIG = 4 };

// Control command bits (1-byte payload of a TYPE_CONTROL frame):
//   bit0 BLE target (1=on)   bit1 apply BLE
//   bit2 Wi-Fi target        bit3 apply Wi-Fi
// Only radios with their "apply" bit set are changed; the new state is persisted
// in NVS and the device reboots to apply it cleanly.
enum {
    CTL_BLE_ON   = 0x01,
    CTL_BLE_SET  = 0x02,
    CTL_WIFI_ON  = 0x04,
    CTL_WIFI_SET = 0x08,
};
#define NVS_NS "hidbridge"

static const uint8_t hid_report_descriptor[] = {
    TUD_HID_REPORT_DESC_KEYBOARD(HID_REPORT_ID(RID_KEYBOARD)),

    HID_USAGE_PAGE ( HID_USAGE_PAGE_DESKTOP     ),
    HID_USAGE      ( HID_USAGE_DESKTOP_MOUSE    ),
    HID_COLLECTION ( HID_COLLECTION_APPLICATION ),
      HID_REPORT_ID ( RID_MOUSE )
      HID_USAGE      ( HID_USAGE_DESKTOP_POINTER ),
      HID_COLLECTION ( HID_COLLECTION_PHYSICAL   ),
        HID_USAGE_PAGE  ( HID_USAGE_PAGE_BUTTON  ),
          HID_USAGE_MIN   ( 1                                      ),
          HID_USAGE_MAX   ( 5                                      ),
          HID_LOGICAL_MIN ( 0                                      ),
          HID_LOGICAL_MAX ( 1                                      ),
          HID_REPORT_COUNT( 5                                      ),
          HID_REPORT_SIZE ( 1                                      ),
          HID_INPUT       ( HID_DATA | HID_VARIABLE | HID_ABSOLUTE ),
          HID_REPORT_COUNT( 1                                      ),
          HID_REPORT_SIZE ( 3                                      ),
          HID_INPUT       ( HID_CONSTANT                           ),
        HID_USAGE_PAGE  ( HID_USAGE_PAGE_DESKTOP ),
          HID_USAGE        ( HID_USAGE_DESKTOP_X     ),
          HID_USAGE        ( HID_USAGE_DESKTOP_Y     ),
          HID_LOGICAL_MIN_N( -32768, 2               ),
          HID_LOGICAL_MAX_N(  32767, 2               ),
          HID_REPORT_SIZE  ( 16                      ),
          HID_REPORT_COUNT ( 2                       ),
          HID_INPUT        ( HID_DATA | HID_VARIABLE | HID_RELATIVE ),
          HID_USAGE        ( HID_USAGE_DESKTOP_WHEEL ),
          HID_LOGICAL_MIN  ( -127                    ),
          HID_LOGICAL_MAX  ( 127                     ),
          HID_REPORT_SIZE  ( 8                       ),
          HID_REPORT_COUNT ( 1                       ),
          HID_INPUT        ( HID_DATA | HID_VARIABLE | HID_RELATIVE ),
      HID_COLLECTION_END,
    HID_COLLECTION_END
};

static const char *hid_string_descriptor[] = {
    (char[]){0x09, 0x04},
    "hidbridge",
    "hidbridge KVM keyboard",
    "0001",
    "hidbridge HID",
};

#define TUSB_DESC_TOTAL_LEN (TUD_CONFIG_DESC_LEN + TUD_HID_DESC_LEN)
static const uint8_t hid_configuration_descriptor[] = {
    // Self-powered (the board is powered independently of the target's port) +
    // remote-wakeup. Declaring self-powered stops the host from policing the USB
    // suspend current budget — a bus-powered claim makes macOS treat the always-on
    // ESP32 as a misbehaving device and refuse to bring its endpoint back on wake.
    TUD_CONFIG_DESCRIPTOR(1, 1, 0, TUSB_DESC_TOTAL_LEN,
                          TUSB_DESC_CONFIG_ATT_SELF_POWERED | TUSB_DESC_CONFIG_ATT_REMOTE_WAKEUP, 100),
    // poll interval 1 ms so the host drains reports at up to 1000 Hz.
    TUD_HID_DESCRIPTOR(0, 4, false, sizeof(hid_report_descriptor), 0x81, 16, 1),
};

uint8_t const *tud_hid_descriptor_report_cb(uint8_t instance)
{
    (void)instance;
    return hid_report_descriptor;
}
uint16_t tud_hid_get_report_cb(uint8_t instance, uint8_t report_id, hid_report_type_t type,
                               uint8_t *buffer, uint16_t reqlen)
{
    (void)instance; (void)report_id; (void)type; (void)buffer; (void)reqlen;
    return 0;
}
void tud_hid_set_report_cb(uint8_t instance, uint8_t report_id, hid_report_type_t type,
                           uint8_t const *buffer, uint16_t bufsize)
{
    (void)instance; (void)report_id; (void)type; (void)buffer; (void)bufsize;
}

// Bus suspend/resume — fired when the target sleeps/wakes. Logged so the monitor
// shows whether a resume actually reaches us when the host is woken by other
// means (its own keyboard, lid, power button); without that resume the device
// stays parked and send_report's remote-wakeup path is what recovers it.
void tud_suspend_cb(bool remote_wakeup_en)
{
    ESP_LOGI(TAG, "USB suspend (remote_wakeup_en=%d)", remote_wakeup_en);
}
void tud_resume_cb(void)
{
    ESP_LOGI(TAG, "USB resume");
}

// ---- Frame parser (reentrant: one state per transport) ----
enum { S_SYNC, S_TYPE, S_LEN, S_PAYLOAD, S_CSUM };
typedef struct {
    uint8_t st, type, payload[128], need, got, csum;
} parser_t;

static SemaphoreHandle_t hid_mtx; // serialize tud_hid_report across transports

static uint8_t payload_len(uint8_t type)
{
    switch (type) {
    case RID_KEYBOARD: return 8;
    case RID_MOUSE:    return 6;
    case TYPE_CONTROL: return 1;
    default:           return 0;
    }
}

// read_flag returns an NVS u8 (radio enable flag), or def if unset.
static uint8_t read_flag(const char *key, uint8_t def)
{
    nvs_handle_t h;
    if (nvs_open(NVS_NS, NVS_READONLY, &h) != ESP_OK) {
        return def;
    }
    uint8_t v = def;
    nvs_get_u8(h, key, &v);
    nvs_close(h);
    return v;
}

// apply_control persists the requested radio enable flags and reboots to apply.
static void apply_control(uint8_t cmd)
{
    uint8_t ble = read_flag("ble_en", 1);
    uint8_t wifi = read_flag("wifi_en", 1);
    if (cmd & CTL_BLE_SET) {
        ble = (cmd & CTL_BLE_ON) ? 1 : 0;
    }
    if (cmd & CTL_WIFI_SET) {
        wifi = (cmd & CTL_WIFI_ON) ? 1 : 0;
    }
    nvs_handle_t h;
    if (nvs_open(NVS_NS, NVS_READWRITE, &h) == ESP_OK) {
        nvs_set_u8(h, "ble_en", ble);
        nvs_set_u8(h, "wifi_en", wifi);
        nvs_commit(h);
        nvs_close(h);
    }
    ESP_LOGI(TAG, "radios: ble=%d wifi=%d -> reboot", ble, wifi);
    esp_restart();
}

// copy_str copies src into dst (size dstsz), always NUL-terminated, truncating if
// needed — without the format/stringop-truncation warnings of snprintf/strncpy.
static void copy_str(char *dst, size_t dstsz, const char *src)
{
    size_t n = strnlen(src, dstsz - 1);
    memcpy(dst, src, n);
    dst[n] = '\0';
}

// load_wifi_creds fills g_ssid/g_pass from NVS, falling back to the .env/Kconfig
// compile-time defaults when NVS has none.
static void load_wifi_creds(void)
{
    copy_str(g_ssid, sizeof(g_ssid), WIFI_SSID);
    copy_str(g_pass, sizeof(g_pass), WIFI_PASSWORD);
    nvs_handle_t h;
    if (nvs_open(NVS_NS, NVS_READONLY, &h) != ESP_OK) {
        return;
    }
    size_t n = sizeof(g_ssid);
    nvs_get_str(h, "wifi_ssid", g_ssid, &n); // unchanged if key absent
    n = sizeof(g_pass);
    nvs_get_str(h, "wifi_pass", g_pass, &n);
    nvs_close(h);
}

// apply_config stores Wi-Fi credentials from a config frame and reboots.
// payload: ssidLen, ssid[ssidLen], passLen, pass[passLen].
static void apply_config(const uint8_t *p, uint8_t len)
{
    if (len < 2) {
        return;
    }
    uint8_t sl = p[0];
    if (1 + sl + 1 > len) {
        return;
    }
    uint8_t pl = p[1 + sl];
    if (1 + sl + 1 + pl > len || sl > 32 || pl > 63) {
        return;
    }
    char ssid[33] = {0}, pass[64] = {0};
    memcpy(ssid, p + 1, sl);
    memcpy(pass, p + 1 + sl + 1, pl);

    nvs_handle_t h;
    if (nvs_open(NVS_NS, NVS_READWRITE, &h) == ESP_OK) {
        nvs_set_str(h, "wifi_ssid", ssid);
        nvs_set_str(h, "wifi_pass", pass);
        nvs_set_u8(h, "wifi_en", 1); // setting creds implies wanting Wi-Fi on
        nvs_commit(h);
        nvs_close(h);
    }
    ESP_LOGI(TAG, "wifi creds saved (ssid=\"%s\") -> reboot", ssid);
    esp_restart();
}

static void send_report(uint8_t type, const uint8_t *payload)
{
    xSemaphoreTake(hid_mtx, portMAX_DELAY);
    // Host asleep => the USB bus is suspended and tud_hid_ready() stays false.
    // Ask the host to resume (works because the config descriptor advertises
    // REMOTE_WAKEUP and the host enabled it), then wait for the bus to come
    // back so this very keypress/click is what wakes it instead of being lost.
    if (tud_suspended()) {
        tud_remote_wakeup();
        for (int i = 0; i < 1000 && tud_suspended(); i++) vTaskDelay(pdMS_TO_TICKS(1));
    }
    for (int i = 0; i < 50 && !tud_hid_ready(); i++) vTaskDelay(pdMS_TO_TICKS(1));
    if (tud_hid_ready()) tud_hid_report(type, payload, payload_len(type));
    xSemaphoreGive(hid_mtx);
}

static void feed(parser_t *p, uint8_t b)
{
    switch (p->st) {
    case S_SYNC:
        if (b == 0xAB) p->st = S_TYPE;
        break;
    case S_TYPE:
        p->type = b;
        p->csum = b;
        p->got = 0;
        if (b == TYPE_CONFIG) { p->st = S_LEN; break; } // length-prefixed
        p->need = payload_len(b);
        if (p->need == 0) { p->st = S_SYNC; break; }
        p->st = S_PAYLOAD;
        break;
    case S_LEN:
        p->need = b; p->csum ^= b;
        if (p->need > sizeof(p->payload)) { p->st = S_SYNC; break; }
        p->st = (p->need == 0) ? S_CSUM : S_PAYLOAD;
        break;
    case S_PAYLOAD:
        p->payload[p->got++] = b; p->csum ^= b;
        if (p->got == p->need) p->st = S_CSUM;
        break;
    case S_CSUM:
        if (b == p->csum) {
            if (p->type == TYPE_CONTROL) {
                apply_control(p->payload[0]); // persists + reboots
            } else if (p->type == TYPE_CONFIG) {
                apply_config(p->payload, p->need); // persists + reboots
            } else {
                send_report(p->type, p->payload);
            }
        }
        p->st = S_SYNC;
        break;
    }
}

// ---- UART transport ----
static void uart_task(void *arg)
{
    const uart_config_t cfg = {
        .baud_rate = DATA_BAUD,
        .data_bits = UART_DATA_8_BITS,
        .parity    = UART_PARITY_DISABLE,
        .stop_bits = UART_STOP_BITS_1,
        .flow_ctrl = UART_HW_FLOWCTRL_DISABLE,
        .source_clk = UART_SCLK_DEFAULT,
    };
    ESP_ERROR_CHECK(uart_driver_install(DATA_UART, DATA_RX_BUF, 0, 0, NULL, 0));
    ESP_ERROR_CHECK(uart_param_config(DATA_UART, &cfg));
    ESP_ERROR_CHECK(uart_set_pin(DATA_UART, DATA_TX_PIN, DATA_RX_PIN,
                                 UART_PIN_NO_CHANGE, UART_PIN_NO_CHANGE));

    parser_t p = { .st = S_SYNC };
    uint8_t buf[256];
    for (;;) {
        // Short timeout (1 ms at 1 kHz tick) so frames are processed promptly
        // instead of being batched into 10-20 ms clumps.
        int n = uart_read_bytes(DATA_UART, buf, sizeof(buf), pdMS_TO_TICKS(1));
        for (int i = 0; i < n; i++) feed(&p, buf[i]);
    }
}

// ---- BLE transport (Nordic-UART-style: write to RX characteristic) ----
// Service 6E400001-..., RX (write) 6E400002-...  (UUID bytes are little-endian).
static const ble_uuid128_t nus_svc_uuid = BLE_UUID128_INIT(
    0x9e, 0xca, 0xdc, 0x24, 0x0e, 0xe5, 0xa9, 0xe0,
    0x93, 0xf3, 0xa3, 0xb5, 0x01, 0x00, 0x40, 0x6e);
static const ble_uuid128_t nus_rx_uuid = BLE_UUID128_INIT(
    0x9e, 0xca, 0xdc, 0x24, 0x0e, 0xe5, 0xa9, 0xe0,
    0x93, 0xf3, 0xa3, 0xb5, 0x02, 0x00, 0x40, 0x6e);

// Received BLE bytes are handed to a dedicated task via this stream buffer, so
// the NimBLE host task is never blocked by the (potentially blocking) USB send
// in send_report — otherwise the whole BLE link stalls and input lags.
static StreamBufferHandle_t ble_sb;

static int gatt_rx_cb(uint16_t conn, uint16_t attr, struct ble_gatt_access_ctxt *ctxt, void *arg)
{
    if (ctxt->op == BLE_GATT_ACCESS_OP_WRITE_CHR) {
        for (struct os_mbuf *om = ctxt->om; om; om = SLIST_NEXT(om, om_next)) {
            xStreamBufferSend(ble_sb, om->om_data, om->om_len, 0);
        }
    }
    return 0;
}

static void ble_consumer_task(void *arg)
{
    parser_t p = { .st = S_SYNC };
    uint8_t buf[128];
    for (;;) {
        size_t n = xStreamBufferReceive(ble_sb, buf, sizeof(buf), portMAX_DELAY);
        for (size_t i = 0; i < n; i++) feed(&p, buf[i]);
    }
}

static const struct ble_gatt_svc_def gatt_svcs[] = {
    {
        .type = BLE_GATT_SVC_TYPE_PRIMARY,
        .uuid = &nus_svc_uuid.u,
        .characteristics = (struct ble_gatt_chr_def[]){
            {
                .uuid = &nus_rx_uuid.u,
                .access_cb = gatt_rx_cb,
                .flags = BLE_GATT_CHR_F_WRITE | BLE_GATT_CHR_F_WRITE_NO_RSP,
            },
            {0},
        },
    },
    {0},
};

static uint8_t ble_addr_type;
static uint16_t s_conn_handle = BLE_HS_CONN_HANDLE_NONE;
static esp_timer_handle_t s_param_timer;
static void ble_advertise(void);

// Ask the central for a fast, fixed 7.5 ms connection interval. Sent ~1s after
// connect because many centrals (incl. Linux/BlueZ) ignore an update requested
// too early. Without this the link sits at BlueZ's lazy 30-50 ms default.
static void request_fast_interval(void *arg)
{
    if (s_conn_handle == BLE_HS_CONN_HANDLE_NONE) return;
    struct ble_gap_upd_params p = {
        .itvl_min = 6, .itvl_max = 6, .latency = 0, .supervision_timeout = 400,
    };
    int rc = ble_gap_update_params(s_conn_handle, &p);
    ESP_LOGI(TAG, "request 7.5ms interval: rc=%d", rc);
}

static int gap_event(struct ble_gap_event *event, void *arg)
{
    switch (event->type) {
    case BLE_GAP_EVENT_CONNECT:
        if (event->connect.status != 0) {
            ble_advertise();
        } else {
            ESP_LOGI(TAG, "BLE connected");
            s_conn_handle = event->connect.conn_handle;
            esp_timer_stop(s_param_timer);
            esp_timer_start_once(s_param_timer, 1000000); // 1s
        }
        return 0;
    case BLE_GAP_EVENT_CONN_UPDATE:
        ESP_LOGI(TAG, "conn params updated (status=%d)", event->conn_update.status);
        return 0;
    case BLE_GAP_EVENT_DISCONNECT:
        ESP_LOGI(TAG, "BLE disconnected");
        s_conn_handle = BLE_HS_CONN_HANDLE_NONE;
        ble_advertise(); // a mid-frame leftover resyncs on the next sync byte
        return 0;
    case BLE_GAP_EVENT_ADV_COMPLETE:
        ble_advertise();
        return 0;
    default:
        return 0;
    }
}

static void ble_advertise(void)
{
    struct ble_hs_adv_fields fields = {0};
    const char *name = ble_svc_gap_device_name();
    fields.name = (uint8_t *)name;
    fields.name_len = strlen(name);
    fields.name_is_complete = 1;
    fields.flags = BLE_HS_ADV_F_DISC_GEN | BLE_HS_ADV_F_BREDR_UNSUP;
    ble_gap_adv_set_fields(&fields);

    struct ble_gap_adv_params adv = {
        .conn_mode = BLE_GAP_CONN_MODE_UND,
        .disc_mode = BLE_GAP_DISC_MODE_GEN,
    };
    ble_gap_adv_start(ble_addr_type, NULL, BLE_HS_FOREVER, &adv, gap_event, NULL);
}

static void ble_on_sync(void)
{
    // Make sure an identity address is configured before advertising — without
    // this ble_hs_id_infer_auto() can fail and advertising never starts.
    int rc = ble_hs_util_ensure_addr(0);
    if (rc != 0) {
        ESP_LOGE(TAG, "ble_hs_util_ensure_addr failed: %d", rc);
        return;
    }
    rc = ble_hs_id_infer_auto(0, &ble_addr_type);
    if (rc != 0) {
        ESP_LOGE(TAG, "infer addr failed: %d", rc);
        return;
    }
    ble_advertise();
    ESP_LOGI(TAG, "BLE advertising as \"%s\"", ble_svc_gap_device_name());
}

static void ble_host_task(void *param)
{
    nimble_port_run();
    nimble_port_freertos_deinit();
}

static void ble_init(void)
{
    ble_sb = xStreamBufferCreate(1024, 1);
    xTaskCreate(ble_consumer_task, "ble_rx", 4096, NULL, 5, NULL);

    const esp_timer_create_args_t targs = {
        .callback = request_fast_interval, .name = "fastparam",
    };
    ESP_ERROR_CHECK(esp_timer_create(&targs, &s_param_timer));

    ESP_ERROR_CHECK(nimble_port_init());
    ble_svc_gap_init();
    ble_svc_gatt_init();
    ESP_ERROR_CHECK(ble_gatts_count_cfg(gatt_svcs));
    ESP_ERROR_CHECK(ble_gatts_add_svcs(gatt_svcs));
    ESP_ERROR_CHECK(ble_svc_gap_device_name_set(CONFIG_HIDBRIDGE_BLE_NAME));
    ble_hs_cfg.sync_cb = ble_on_sync;
    nimble_port_freertos_init(ble_host_task);
}

// ---- Wi-Fi + TCP transport (STA, mDNS hidbridge.local, port 3232) ----
static EventGroupHandle_t wifi_eg;
#define WIFI_CONNECTED_BIT BIT0

static void wifi_event_handler(void *arg, esp_event_base_t base, int32_t id, void *data)
{
    if (base == WIFI_EVENT && id == WIFI_EVENT_STA_START) {
        esp_wifi_connect();
    } else if (base == WIFI_EVENT && id == WIFI_EVENT_STA_DISCONNECTED) {
        xEventGroupClearBits(wifi_eg, WIFI_CONNECTED_BIT);
        esp_wifi_connect();
    } else if (base == IP_EVENT && id == IP_EVENT_STA_GOT_IP) {
        ip_event_got_ip_t *e = (ip_event_got_ip_t *)data;
        ESP_LOGI(TAG, "got IP " IPSTR " (%s.local)", IP2STR(&e->ip_info.ip), CONFIG_HIDBRIDGE_MDNS_HOSTNAME);
        xEventGroupSetBits(wifi_eg, WIFI_CONNECTED_BIT);
    }
}

static void wifi_connect_blocking(void)
{
    wifi_eg = xEventGroupCreate();
    ESP_ERROR_CHECK(esp_netif_init());
    ESP_ERROR_CHECK(esp_event_loop_create_default());
    esp_netif_create_default_wifi_sta();
    wifi_init_config_t cfg = WIFI_INIT_CONFIG_DEFAULT();
    ESP_ERROR_CHECK(esp_wifi_init(&cfg));
    ESP_ERROR_CHECK(esp_event_handler_instance_register(WIFI_EVENT, ESP_EVENT_ANY_ID,
                                                        wifi_event_handler, NULL, NULL));
    ESP_ERROR_CHECK(esp_event_handler_instance_register(IP_EVENT, IP_EVENT_STA_GOT_IP,
                                                        wifi_event_handler, NULL, NULL));
    wifi_config_t wc = {0};
    memcpy(wc.sta.ssid, g_ssid, strnlen(g_ssid, sizeof(wc.sta.ssid)));
    memcpy(wc.sta.password, g_pass, strnlen(g_pass, sizeof(wc.sta.password)));
    ESP_ERROR_CHECK(esp_wifi_set_mode(WIFI_MODE_STA));
    ESP_ERROR_CHECK(esp_wifi_set_config(WIFI_IF_STA, &wc));
    ESP_ERROR_CHECK(esp_wifi_start());
    ESP_ERROR_CHECK(esp_wifi_set_ps(WIFI_PS_NONE)); // no modem sleep: lower latency
    xEventGroupWaitBits(wifi_eg, WIFI_CONNECTED_BIT, pdFALSE, pdTRUE, portMAX_DELAY);

    ESP_ERROR_CHECK(mdns_init());
    ESP_ERROR_CHECK(mdns_hostname_set(CONFIG_HIDBRIDGE_MDNS_HOSTNAME));
    mdns_instance_name_set("hidbridge KVM bridge");
    mdns_service_add(NULL, "_gohid", "_tcp", CONFIG_HIDBRIDGE_TCP_PORT, NULL, 0);
}

static void net_task(void *arg)
{
    wifi_connect_blocking();

    int ls = socket(AF_INET, SOCK_STREAM, IPPROTO_TCP);
    int opt = 1;
    setsockopt(ls, SOL_SOCKET, SO_REUSEADDR, &opt, sizeof(opt));
    struct sockaddr_in a = {
        .sin_family = AF_INET,
        .sin_addr.s_addr = htonl(INADDR_ANY),
        .sin_port = htons(CONFIG_HIDBRIDGE_TCP_PORT),
    };
    if (bind(ls, (struct sockaddr *)&a, sizeof(a)) != 0 || listen(ls, 1) != 0) {
        ESP_LOGE(TAG, "tcp bind/listen failed: errno %d", errno);
        vTaskDelete(NULL);
        return;
    }
    ESP_LOGI(TAG, "TCP server on port %d", CONFIG_HIDBRIDGE_TCP_PORT);

    uint8_t buf[256];
    for (;;) {
        int cs = accept(ls, NULL, NULL);
        if (cs < 0) continue;
        int one = 1;
        setsockopt(cs, IPPROTO_TCP, TCP_NODELAY, &one, sizeof(one));
        ESP_LOGI(TAG, "client connected");
        parser_t p = { .st = S_SYNC };
        for (;;) {
            int n = recv(cs, buf, sizeof(buf), 0);
            if (n <= 0) break;
            for (int i = 0; i < n; i++) feed(&p, buf[i]);
        }
        close(cs);
        ESP_LOGI(TAG, "client disconnected");
    }
}

// Wi-Fi is started only if an SSID was configured; otherwise BLE + cable only.
static bool wifi_configured(void)
{
    return g_ssid[0] != '\0' && strcmp(g_ssid, "changeme") != 0;
}

void app_main(void)
{
    ESP_ERROR_CHECK(nvs_flash_init());
    hid_mtx = xSemaphoreCreateMutex();

    const tinyusb_config_t tusb_cfg = {
        .device_descriptor = NULL,
        .string_descriptor = hid_string_descriptor,
        .string_descriptor_count = sizeof(hid_string_descriptor) / sizeof(hid_string_descriptor[0]),
        .external_phy = false,
#if (TUD_OPT_HIGH_SPEED)
        .fs_configuration_descriptor = hid_configuration_descriptor,
        .hs_configuration_descriptor = hid_configuration_descriptor,
        .qualifier_descriptor = NULL,
#else
        .configuration_descriptor = hid_configuration_descriptor,
#endif
    };
    ESP_ERROR_CHECK(tinyusb_driver_install(&tusb_cfg));

    xTaskCreate(uart_task, "uart", 4096, NULL, 5, NULL); // cable always on (also the control channel)

    load_wifi_creds(); // NVS overrides the compile-time .env defaults

    if (read_flag("ble_en", 1)) {                        // BLE unless disabled via -set-ble off
        ble_init();
    }
    if (read_flag("wifi_en", 1) && wifi_configured()) {  // Wi-Fi if enabled and SSID set
        xTaskCreate(net_task, "net", 4096, NULL, 5, NULL);
    }
}
