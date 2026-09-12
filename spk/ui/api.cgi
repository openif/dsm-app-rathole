#!/bin/sh
# ==============================================================================
# api.cgi - DSM FastCGI 反向代理桥接网关
# ------------------------------------------------------------------------------
# 作用:
#   在 DSM 7.x 桌面原生内嵌窗口 (WebMan 3rdparty) 下接收前端请求，
#   安全校验请求参数与负载后，转发给本地运行的 rathole-ui 守护进程 (127.0.0.1:8520)。
#   支持局域网、公网 IP 以及 QuickConnect 云端穿透环境无缝使用。
# ==============================================================================

# 设置安全的 umask
umask 077

QUERY="${QUERY_STRING:-}"
ACTION=$(printf '%s' "$QUERY" | tr '&' '\n' | sed -n 's/^action=//p' | head -n 1)
[ -z "$ACTION" ] && ACTION="status"

# 1. 严格白名单校验 action 参数，防止非法路由
case "$ACTION" in
    status|config|action|log|clear_log)
        ;;
    *)
        printf 'Status: 400 Bad Request\r\n'
        printf 'Content-Type: application/json; charset=UTF-8\r\n'
        printf 'X-Content-Type-Options: nosniff\r\n\r\n'
        printf '{"success":false,"message":"无效的操作指令"}\n'
        exit 0
        ;;
esac

TARGET="http://127.0.0.1:8520/api/${ACTION}"

# 2. 校验并限制可选参数（如 log 行数），防止参数注入与 DoS
if [ "$ACTION" = "log" ]; then
    LINES=$(printf '%s' "$QUERY" | tr '&' '\n' | sed -n 's/^lines=//p' | head -n 1)
    case "$LINES" in
        ''|*[!0-9]*)
            LINES="500"
            ;;
        *)
            if [ "$LINES" -gt 2000 ] 2>/dev/null; then
                LINES="2000"
            elif [ "$LINES" -lt 1 ] 2>/dev/null; then
                LINES="100"
            fi
            ;;
    esac
    TARGET="${TARGET}?lines=${LINES}"
fi

# 3. 防范超大文件上传攻击 (限制请求体最大 2MB)
MAX_BODY_SIZE=2097152
if [ -n "${CONTENT_LENGTH:-}" ] && [ "$CONTENT_LENGTH" -gt "$MAX_BODY_SIZE" ] 2>/dev/null; then
    printf 'Status: 413 Payload Too Large\r\n'
    printf 'Content-Type: application/json; charset=UTF-8\r\n'
    printf 'X-Content-Type-Options: nosniff\r\n\r\n'
    printf '{"success":false,"message":"请求体超过最大允许限制 (2MB)"}\n'
    exit 0
fi

# 4. 注册进程安全的临时文件与清理陷阱 (Trap)
TMP_POST="/tmp/rathole_post_$$.json"
CURL_ERR="/tmp/rathole_curl_err_$$.log"
trap 'rm -f "$TMP_POST" "$CURL_ERR"' EXIT INT TERM

# 输出标准响应头
printf 'Content-Type: application/json; charset=UTF-8\r\n'
printf 'X-Content-Type-Options: nosniff\r\n\r\n'

if [ "${REQUEST_METHOD:-GET}" = "POST" ]; then
    if [ -n "${CONTENT_LENGTH:-}" ] && [ "$CONTENT_LENGTH" -gt 0 ] 2>/dev/null; then
        # 按照 CONTENT_LENGTH 严格读取标准输入，防止 FastCGI 管道挂起 (EOF hang)
        head -c "$CONTENT_LENGTH" > "$TMP_POST" 2>/dev/null || dd bs=1 count="$CONTENT_LENGTH" of="$TMP_POST" 2>/dev/null
    else
        : > "$TMP_POST"
    fi

    RESP=$(curl -s -m 8 -X POST -H "Content-Type: application/json" --data-binary @"$TMP_POST" "${TARGET}" 2>"$CURL_ERR")
    CURL_STATUS=$?

    if [ $CURL_STATUS -ne 0 ] || [ -z "$RESP" ]; then
        ERR_DETAIL=""
        [ -s "$CURL_ERR" ] && ERR_DETAIL=": $(cat "$CURL_ERR")"
        printf '{"success":false,"message":"Rathole-UI 守护服务未响应 (curl 状态码 %s)%s"}\n' "$CURL_STATUS" "$ERR_DETAIL"
    else
        printf '%s\n' "$RESP"
    fi
else
    RESP=$(curl -s -m 8 "${TARGET}" 2>"$CURL_ERR")
    CURL_STATUS=$?
    if [ $CURL_STATUS -ne 0 ] || [ -z "$RESP" ]; then
        if [ "$ACTION" = "status" ]; then
            printf '{"running":false,"pid":0,"uptime":"-","last_error":"Rathole-UI 守护服务未运行"}\n'
        elif [ "$ACTION" = "config" ]; then
            printf '{"success":false,"content":"","message":"Rathole-UI 守护服务未运行"}\n'
        else
            printf '{"success":false,"message":"Rathole-UI 守护服务未运行"}\n'
        fi
    else
        printf '%s\n' "$RESP"
    fi
fi

exit 0
