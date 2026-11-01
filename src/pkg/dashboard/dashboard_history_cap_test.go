package dashboard

import (
	"strings"
	"testing"
)

func TestDashboardHistoryCapHelpersBounded(t *testing.T) {
	html := indexHTML(t)
	for _, want := range []string{
		"const CLIENT_HISTORY_MAX_SAMPLES = ",
		"function capHistory(arr, max)",
		"historyData = capHistory(evalEntries || [])",
		"const SPARKLINE_KEY_CACHE_MAX = ",
		"function rememberSparklineKey(id, key)",
	} {
		if !strings.Contains(html, want) {
			t.Fatalf("history cap contract missing %q", want)
		}
	}
	merge := jsFunctionBody(t, html, "function mergeTrendHistory()")
	if !strings.Contains(merge, "capHistory(historyData)") {
		t.Fatal("mergeTrendHistory does not cap historyData after injecting trend points")
	}
	replay := jsFunctionBody(t, html, "function replaySparklinesIn(root, opts)")
	if strings.Contains(replay, "_sparklineLastKeys.set(") || !strings.Contains(replay, "rememberSparklineKey(id, key)") {
		t.Fatal("replaySparklinesIn must record keys through the bounded rememberSparklineKey helper")
	}
	animate := jsFunctionBody(t, html, "function animateSparklineSvg(svg, length)")
	if !strings.Contains(animate, "svg.isConnected") {
		t.Fatal("animateSparklineSvg must skip svgs detached by a re-render")
	}
}

func TestDashboardLiveConnectionsCloseBeforeReconnect(t *testing.T) {
	html := indexHTML(t)
	if got := strings.Count(html, "new EventSource('/api/events')"); got != 1 {
		t.Fatalf("EventSource('/api/events') count = %d, want 1", got)
	}
	conn := jsFunctionBody(t, html, "function connect()")
	for _, want := range []string{
		"clearTimeout(_eventSourceReconnectTimer)",
		"_eventSource.close()",
		"if (!_eventSourceReconnectTimer)",
	} {
		if !strings.Contains(conn, want) {
			t.Fatalf("connect() missing %q", want)
		}
	}
	jam := jsFunctionBody(t, html, "function connectCampaignJamLive(id)")
	for _, want := range []string{"prev.onclose = null", "prev.close()"} {
		if !strings.Contains(jam, want) {
			t.Fatalf("connectCampaignJamLive missing %q", want)
		}
	}
}
