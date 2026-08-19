package tui

import (
	"os"
	"strings"
)

// Page identifies one top-level TUI route.
type Page string

const (
	PageSessions    Page = "sessions"
	PageDashboard   Page = "dashboard"
	PageUsage       Page = "usage"
	PageActivity    Page = "activity"
	PageTrends      Page = "trends"
	PageInsights    Page = "insights"
	PagePinned      Page = "pinned"
	PageTrash       Page = "trash"
	PageRecentEdits Page = "recent-edits"
	PageSettings    Page = "settings"
)

var pages = []Page{
	PageSessions, PageDashboard, PageUsage, PageActivity, PageTrends,
	PageInsights, PagePinned, PageTrash, PageRecentEdits, PageSettings,
}

type stringsTable struct {
	App, Loading, NoResults, Help, Command, Search, Error, ReadOnly string
	PageNames                                                       map[Page]string
}

func systemStrings() stringsTable {
	lang := strings.ToLower(firstNonEmpty(os.Getenv("LC_ALL"), os.Getenv("LC_MESSAGES"), os.Getenv("LANG")))
	switch {
	case strings.HasPrefix(lang, "zh_tw"), strings.HasPrefix(lang, "zh-tw"), strings.HasPrefix(lang, "zh_hk"):
		return translatedStrings("AgentsView", "載入中…", "無結果", "說明", "指令", "搜尋", "錯誤", "唯讀", []string{"工作階段", "儀表板", "用量", "活動", "趨勢", "洞見", "釘選", "回收站", "最近編輯", "設定"})
	case strings.HasPrefix(lang, "zh"):
		return translatedStrings("AgentsView", "加载中…", "无结果", "帮助", "命令", "搜索", "错误", "只读", []string{"会话", "仪表盘", "用量", "活动", "趋势", "洞察", "已固定", "回收站", "最近编辑", "设置"})
	case strings.HasPrefix(lang, "ko"):
		return translatedStrings("AgentsView", "불러오는 중…", "결과 없음", "도움말", "명령", "검색", "오류", "읽기 전용", []string{"세션", "대시보드", "사용량", "활동", "트렌드", "인사이트", "고정됨", "휴지통", "최근 편집", "설정"})
	default:
		return translatedStrings("AgentsView", "Loading…", "No results", "Help", "Command", "Search", "Error", "read-only", []string{"Sessions", "Dashboard", "Usage", "Activity", "Trends", "Insights", "Pinned", "Trash", "Recent edits", "Settings"})
	}
}

func translatedStrings(app, loading, none, help, command, search, errLabel, readOnly string, names []string) stringsTable {
	pageNames := make(map[Page]string, len(pages))
	for i, page := range pages {
		pageNames[page] = names[i]
	}
	return stringsTable{App: app, Loading: loading, NoResults: none, Help: help, Command: command, Search: search, Error: errLabel, ReadOnly: readOnly, PageNames: pageNames}
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return "en"
}
