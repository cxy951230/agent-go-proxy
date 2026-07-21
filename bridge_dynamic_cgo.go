//go:build cgo && !windows

package main

/*
#cgo darwin LDFLAGS: -ldl
#cgo linux LDFLAGS: -ldl
#include <dlfcn.h>
#include <stddef.h>
#include <stdint.h>
#include <stdlib.h>
#include <string.h>
#include <stdio.h>

typedef int (*login_start_fn)(const uint8_t *, size_t, void **, char **);
typedef int (*login_wait_fn)(void *, char **);
typedef int (*login_cancel_fn)(void *);
typedef void (*session_free_fn)(void *);
typedef void (*string_free_fn)(char *);
typedef int (*status_query_fn)(const uint8_t *, size_t, const uint8_t *, size_t, char **);
typedef int (*token_refresh_fn)(const uint8_t *, size_t, char **);
typedef int (*models_list_fn)(const uint8_t *, size_t, char **);
typedef int (*responses_schema_fn)(char **);
typedef const char *(*last_error_fn)(void);

static void *bridge_handle;
static login_start_fn bridge_login_start;
static login_wait_fn bridge_login_wait;
static login_cancel_fn bridge_login_cancel;
static session_free_fn bridge_session_free;
static string_free_fn bridge_string_free;
static status_query_fn bridge_status_query;
static token_refresh_fn bridge_token_refresh;
static models_list_fn bridge_models_list;
static responses_schema_fn bridge_responses_schema;
static last_error_fn bridge_last_error;
static char bridge_error[1024];

static int bridge_symbol(void **out, const char *name) {
    *out = dlsym(bridge_handle, name);
    if (*out == NULL) {
        snprintf(bridge_error, sizeof(bridge_error), "missing Bridge symbol: %s", name);
        return 0;
    }
    return 1;
}

static int bridge_load(const char *path) {
    if (bridge_handle != NULL) return 0;
    bridge_handle = dlopen(path, RTLD_NOW | RTLD_LOCAL);
    if (bridge_handle == NULL) {
        snprintf(bridge_error, sizeof(bridge_error), "dlopen failed: %s", dlerror());
        return 1;
    }
    if (!bridge_symbol((void **)&bridge_login_start, "codex_bridge_login_start") ||
        !bridge_symbol((void **)&bridge_login_wait, "codex_bridge_login_wait") ||
        !bridge_symbol((void **)&bridge_login_cancel, "codex_bridge_login_cancel") ||
        !bridge_symbol((void **)&bridge_session_free, "codex_bridge_login_session_free") ||
        !bridge_symbol((void **)&bridge_string_free, "codex_bridge_string_free") ||
        !bridge_symbol((void **)&bridge_status_query, "codex_bridge_status_query") ||
        !bridge_symbol((void **)&bridge_token_refresh, "codex_bridge_token_refresh") ||
        !bridge_symbol((void **)&bridge_models_list, "codex_bridge_models_list") ||
        !bridge_symbol((void **)&bridge_responses_schema, "codex_bridge_responses_schema") ||
        !bridge_symbol((void **)&bridge_last_error, "codex_bridge_login_last_error")) {
        dlclose(bridge_handle);
        bridge_handle = NULL;
        return 1;
    }
    return 0;
}

static const char *bridge_load_error(void) { return bridge_error; }
static int bridge_call_start(const uint8_t *config, size_t config_len, void **session, char **out) {
    return bridge_login_start(config, config_len, session, out);
}
static int bridge_call_wait(void *session, char **out) { return bridge_login_wait(session, out); }
static int bridge_call_cancel(void *session) { return bridge_login_cancel(session); }
static void bridge_call_session_free(void *session) { bridge_session_free(session); }
static int bridge_call_status(const uint8_t *auth, size_t auth_len, const uint8_t *base, size_t base_len, char **out) {
    return bridge_status_query(auth, auth_len, base, base_len, out);
}
static int bridge_call_token_refresh(const uint8_t *auth, size_t auth_len, char **out) {
    return bridge_token_refresh(auth, auth_len, out);
}
static int bridge_call_models_list(const uint8_t *auth, size_t auth_len, char **out) {
    return bridge_models_list(auth, auth_len, out);
}
static int bridge_call_responses_schema(char **out) {
    return bridge_responses_schema(out);
}
static const char *bridge_call_last_error(void) { return bridge_last_error(); }
static void bridge_call_string_free(char *value) { bridge_string_free(value); }
*/
import "C"

import (
	"errors"
	"fmt"
	"sync"
	"unsafe"
)

type nativeBridge struct {
	path string
	once sync.Once
	err  error
}

func newNativeBridge(path string) *nativeBridge {
	return &nativeBridge{path: path}
}

func (b *nativeBridge) load() error {
	b.once.Do(func() {
		path := C.CString(b.path)
		defer C.free(unsafe.Pointer(path))
		if code := C.bridge_load(path); code != 0 {
			b.err = errors.New(C.GoString(C.bridge_load_error()))
		}
	})
	return b.err
}

func bridgeError(code C.int) error {
	if code == 0 {
		return nil
	}
	message := C.GoString(C.bridge_call_last_error())
	if message == "" {
		message = fmt.Sprintf("Bridge call failed with code %d", int(code))
	}
	return errors.New(message)
}

func (b *nativeBridge) loginStart(config []byte) (unsafe.Pointer, string, error) {
	if err := b.load(); err != nil {
		return nil, "", err
	}
	var session unsafe.Pointer
	var output *C.char
	var configPtr *C.uint8_t
	if len(config) > 0 {
		configPtr = (*C.uint8_t)(unsafe.Pointer(&config[0]))
	}
	code := C.bridge_call_start(configPtr, C.size_t(len(config)), &session, &output)
	if err := bridgeError(code); err != nil {
		return nil, "", err
	}
	return session, takeBridgeString(output), nil
}

func (b *nativeBridge) loginWait(session unsafe.Pointer) (string, error) {
	var output *C.char
	code := C.bridge_call_wait(session, &output)
	if err := bridgeError(code); err != nil {
		return "", err
	}
	return takeBridgeString(output), nil
}

func (b *nativeBridge) loginCancel(session unsafe.Pointer) error {
	return bridgeError(C.bridge_call_cancel(session))
}

func (b *nativeBridge) sessionFree(session unsafe.Pointer) {
	if session != nil {
		C.bridge_call_session_free(session)
	}
}

func (b *nativeBridge) status(auth, baseURL []byte) (string, error) {
	if err := b.load(); err != nil {
		return "", err
	}
	var output *C.char
	var authPtr, basePtr *C.uint8_t
	if len(auth) > 0 {
		authPtr = (*C.uint8_t)(unsafe.Pointer(&auth[0]))
	}
	if len(baseURL) > 0 {
		basePtr = (*C.uint8_t)(unsafe.Pointer(&baseURL[0]))
	}
	code := C.bridge_call_status(authPtr, C.size_t(len(auth)), basePtr, C.size_t(len(baseURL)), &output)
	if err := bridgeError(code); err != nil {
		return "", err
	}
	return takeBridgeString(output), nil
}

// tokenRefresh 调用 Bridge 刷新凭证。返回的 JSON 描述结果(refreshed/permanent/message/auth),
// 刷新失败本身不是错误码,只有参数非法或库加载失败才返回 error。
func (b *nativeBridge) tokenRefresh(auth []byte) (string, error) {
	if err := b.load(); err != nil {
		return "", err
	}
	var output *C.char
	var authPtr *C.uint8_t
	if len(auth) > 0 {
		authPtr = (*C.uint8_t)(unsafe.Pointer(&auth[0]))
	}
	code := C.bridge_call_token_refresh(authPtr, C.size_t(len(auth)), &output)
	if err := bridgeError(code); err != nil {
		return "", err
	}
	return takeBridgeString(output), nil
}

// modelsList 拉取该账号可用的模型目录(含每个模型各自的推理强度与速度档位)。
func (b *nativeBridge) modelsList(auth []byte) (string, error) {
	if err := b.load(); err != nil {
		return "", err
	}
	var output *C.char
	var authPtr *C.uint8_t
	if len(auth) > 0 {
		authPtr = (*C.uint8_t)(unsafe.Pointer(&auth[0]))
	}
	code := C.bridge_call_models_list(authPtr, C.size_t(len(auth)), &output)
	if err := bridgeError(code); err != nil {
		return "", err
	}
	return takeBridgeString(output), nil
}

// responsesSchema 取 Codex Responses 请求体的 JSON Schema(含各基线字段默认值)。
// 静态元数据,无需凭证、不发网络请求。
func (b *nativeBridge) responsesSchema() (string, error) {
	if err := b.load(); err != nil {
		return "", err
	}
	var output *C.char
	code := C.bridge_call_responses_schema(&output)
	if err := bridgeError(code); err != nil {
		return "", err
	}
	return takeBridgeString(output), nil
}

func takeBridgeString(value *C.char) string {
	if value == nil {
		return ""
	}
	result := C.GoString(value)
	C.bridge_call_string_free(value)
	return result
}
