#!/bin/sh
# Test double for ffprobe. Never used in production and never shipped in the
# runtime image. It records its argv (one argument per line, terminated by
# --END--) into $FFPROBE_CAPTURE so tests can assert the exact ffprobe flags,
# the Basic-auth headers and the WebDAV URL without running real ffprobe.
#
# Modes (via $FFPROBE_MODE):
#   json          (default) emit valid probe JSON, echoing the URL argument in
#                 format.filename so the URL-sanitising code is exercised;
#   http          fetch the probe JSON from the last argv value (an httptest
#                 server) with busybox wget, then print it;
#   bad-json      emit a non-JSON body (invalid data, must not be retried);
#   no-streams    emit valid JSON with an empty streams array;
#   stderr-secret fail immediately, echoing the URL and headers on stderr;
#   always-timeout sleep well past any test timeout.
set -u

capture=${FFPROBE_CAPTURE:-}
if [ -n "$capture" ]; then
	for arg do
		# -headers contains CRLF; keep one capture line per argv entry.
		escaped=$(printf '%s' "$arg" | tr '\015\012' '~~')
		printf '%s\n' "$escaped" >>"$capture"
	done
	printf '%s\n' '--END--' >>"$capture"
fi

# Captures the -headers argument verbatim (CRLF included) so tests can assert
# the exact wire-format header lines instead of merely checking substrings.
header_capture=${FFPROBE_HEADER_CAPTURE:-}
if [ -n "$header_capture" ]; then
	previous=''
	for arg do
		if [ "$previous" = '-headers' ]; then
			printf '%s' "$arg" >"$header_capture"
		fi
		previous=$arg
	done
fi

last=''
for arg do
	last=$arg
done

mode=${FFPROBE_MODE:-json}
case "$mode" in
json)
	printf '{"programs":[],"stream_groups":[],"streams":[{"index":0,"codec_name":"h264","codec_type":"video","duration":"120.000000","width":1920,"height":1080},{"index":1,"codec_name":"aac","codec_type":"audio","duration":"120.000000"}],"format":{"filename":"%s","format_name":"mov,mp4","duration":"120.000000","size":"1048576"}}\n' "$last"
	;;
http)
	# busybox wget only accepts 200, so the httptest server sends 200 on the
	# successful retry (its delayed cold reply is still a 206).
	wget -q -O - "$last"
	;;
timeout-error)
	# ffprobe's own -rw_timeout expired, but without the process being killed.
	printf 'Error opening input file: Operation timed out\n' >&2
	exit 1
	;;
bad-json)
	printf 'this is not ffprobe json\n'
	;;
no-streams)
	printf '{"streams":[],"format":{"filename":"%s","duration":"10.000000","size":"4096"}}\n' "$last"
	;;
echo-url)
	printf '{"format":{"filename":"%s","tags":{"comment":"source: %s"}},"streams":[{"codec_name":"h264","codec_type":"video","duration":"1.0","filename":"%s"}],"chapters":[{"tags":{"url":"%s"}}]}\n' "$last" "$last" "$last" "$last"
	;;
stderr-secret)
	printf 'cannot open %s with headers %s\n' "$last" "${FFPROBE_SECRET_HEADER:-}" >&2
	exit 1
	;;
always-timeout)
	sleep 30
	;;
*)
	printf 'unknown fake ffprobe mode: %s\n' "$mode" >&2
	exit 2
	;;
esac
