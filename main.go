package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"html/template"
	"io"
	"log"
	"net/http"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/go-echarts/go-echarts/v2/charts"
	"github.com/go-echarts/go-echarts/v2/opts"
	"github.com/go-echarts/go-echarts/v2/types"
	_ "github.com/lib/pq"
	"github.com/nbd-wtf/go-nostr"
)

var pageTpl = template.Must(template.New("page").Funcs(template.FuncMap{
	"add": func(a, b int) int { return a + b },
	"lt":  func(a, b int) bool { return a < b },
	"eq":  func(a, b int) bool { return a == b },
	"stripWss": func(url string) string {
		url = strings.TrimPrefix(url, "wss://")
		url = strings.TrimPrefix(url, "ws://")
		return url
	},
}).Parse(`
{{define "header"}}
<!DOCTYPE html>
<html lang="ja">
<head>
  <meta charset="utf-8">
  <title>Nostr Relay Ranking</title>
  <meta name="viewport" content="width=device-width, initial-scale=1.0">
  <script src="https://cdn.tailwindcss.com"></script>
  <link href="https://fonts.googleapis.com/css2?family=Noto+Sans+JP:wght@400;500;700&display=swap" rel="stylesheet">
  <script src="https://go-echarts.github.io/go-echarts-assets/assets/echarts.min.js"></script>
  <script src="https://go-echarts.github.io/go-echarts-assets/assets/themes/macarons.js"></script>
  <style>
    body { font-family: 'Noto Sans JP', sans-serif; }
    .echarts-container { max-width: 1280px; margin: 0 auto; padding: 20px 0; }
  </style>
</head>
<body class="bg-gray-50 dark:bg-gray-900 text-gray-900 dark:text-gray-100 min-h-screen">
<div class="container mx-auto px-4 py-8 max-w-7xl">
  <header class="text-center mb-12">
    <h1 class="text-4xl md:text-6xl font-bold text-indigo-600 dark:text-indigo-400 mb-4">
      Nostr Relay Ranking
    </h1>
    <p class="text-lg md:text-xl text-gray-600 dark:text-gray-300 max-w-4xl mx-auto">
      Nostr の kind 10002（Relay List Metadata）から集計した<br class="hidden md:block">
      現在最も使われているリレーのランキングです（主に日本人ユーザを対象）
    </p>
    <p class="mt-4 text-sm text-gray-500 dark:text-gray-400">
      更新日時: {{.UpdateTime}}
    </p>
  </header>
  <div class="echarts-container">
{{end}}

{{define "footer"}}
  </div>
  <section class="mt-20">
    <h2 class="text-3xl font-bold text-center mb-8 text-indigo-600 dark:text-indigo-400">
      現在の詳細ランキング（利用者数 20人以上）
    </h2>
    <div class="overflow-x-auto rounded-xl shadow-2xl bg-white dark:bg-gray-800">
      <table class="w-full min-w-max table-auto">
        <thead class="bg-gradient-to-r from-indigo-600 to-purple-600 text-white">
          <tr>
            <th class="px-6 py-5 text-left text-sm font-semibold uppercase tracking-wider">順位</th>
            <th class="px-6 py-5 text-left text-sm font-semibold uppercase tracking-wider">リレーURL</th>
            <th class="px-6 py-5 text-left text-sm font-semibold uppercase tracking-wider">説明</th>
            <th class="px-6 py-5 text-right text-sm font-semibold uppercase tracking-wider">利用者数</th>
          </tr>
        </thead>
        <tbody class="divide-y divide-gray-200 dark:divide-gray-700">
          {{range $i, $r := .Ranks}}
          <tr class="{{if lt $i 3}}bg-yellow-50 dark:bg-yellow-900/30{{else}}bg-gray-50 dark:bg-gray-800/50{{end}} hover:bg-gray-100 dark:hover:bg-gray-700 transition">
            <td class="px-6 py-5 font-bold text-lg">
              {{add $i 1}}位
              {{if eq $i 0}}🥇{{else if eq $i 1}}🥈{{else if eq $i 2}}🥉{{end}}
            </td>
            <td class="px-6 py-5 font-mono text-sm break-all">
              <a href="https://njump.compile-error.net/r/{{stripWss $r.Name}}" target="_blank" class="text-indigo-600 dark:text-indigo-400 hover:underline">
                {{$r.Name}}
              </a>
            </td>
            <td class="px-6 py-5 text-sm text-gray-600 dark:text-gray-300 max-w-xl">{{$r.Description}}</td>
            <td class="px-6 py-5 text-right font-bold text-xl text-indigo-600 dark:text-indigo-400">{{$r.Count}}</td>
          </tr>
          {{end}}
        </tbody>
      </table>
    </div>
  </section>

  <footer class="mt-20 text-center text-sm text-gray-500 dark:text-gray-400">
    <p>データは日本のリレーを中心に複数の公開リレーから kind 10002 を収集・重複除去して集計しています（最大1000件/リレー）</p>
    <p class="mt-2">毎日自動更新 • Generated with ❤️ by Go + go-echarts + Tailwind CSS</p>
  </footer>
</div>
</body>
</html>
{{end}}
`))

type Rank struct {
	Name        string
	Count       int
	Description string
}

type RelayInfo struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	Pubkey      string `json:"pubkey"`
	Contact     string `json:"contact"`
}

type pageData struct {
	UpdateTime string
	Ranks      []Rank
}

type myRenderer struct {
	chart *charts.Line
	data  pageData
}

func (r *myRenderer) Render(w io.Writer) error {
	var buf strings.Builder
	if err := r.chart.Render(&buf); err != nil {
		return err
	}
	html := buf.String()

	if err := pageTpl.ExecuteTemplate(w, "header", r.data); err != nil {
		return err
	}

	start := strings.Index(html, "<body>")
	end := strings.LastIndex(html, "</body>")
	if start != -1 && end != -1 {
		chartContent := html[start+6 : end]
		styleStart := strings.Index(chartContent, "<style>")
		if styleStart != -1 {
			styleEnd := strings.Index(chartContent, "</style>")
			if styleEnd != -1 {
				chartContent = chartContent[:styleStart] + chartContent[styleEnd+8:]
			}
		}
		if _, err := w.Write([]byte(chartContent)); err != nil {
			return err
		}
	}

	if err := pageTpl.ExecuteTemplate(w, "footer", r.data); err != nil {
		return err
	}
	return nil
}

func fetchRelayInfo(relayURL string) RelayInfo {
	httpURL := strings.Replace(relayURL, "wss://", "https://", 1)
	httpURL = strings.Replace(httpURL, "ws://", "http://", 1)

	client := &http.Client{Timeout: 5 * time.Second}
	req, err := http.NewRequest("GET", httpURL, nil)
	if err != nil {
		return RelayInfo{}
	}
	req.Header.Set("Accept", "application/nostr+json")

	resp, err := client.Do(req)
	if err != nil {
		return RelayInfo{}
	}
	defer resp.Body.Close()

	var info RelayInfo
	if err := json.NewDecoder(resp.Body).Decode(&info); err != nil {
		return RelayInfo{}
	}
	return info
}

func count(relays []string) map[string]int {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	seen := make(map[string]*nostr.Event)
	var mu sync.Mutex
	var wg sync.WaitGroup

	filter := nostr.Filter{Kinds: []int{10002}, Limit: 1000}

	for _, relay := range relays {
		wg.Add(1)
		go func(rurl string) {
			defer wg.Done()
			relay, err := nostr.RelayConnect(ctx, rurl)
			if err != nil {
				log.Printf("connect error %s: %v", rurl, err)
				return
			}
			defer relay.Close()

			events, err := relay.QuerySync(ctx, filter)
			if err != nil {
				log.Printf("query error %s: %v", rurl, err)
				return
			}

			mu.Lock()
			for _, ev := range events {
				if old, ok := seen[ev.PubKey]; !ok || old.CreatedAt < ev.CreatedAt {
					seen[ev.PubKey] = ev
				}
			}
			mu.Unlock()
			log.Printf("%s → %d events", rurl, len(events))
		}(relay)
	}
	wg.Wait()

	result := make(map[string]int)
	wssurls := []string{}
	valid := false
	for _, ev := range seen {
		for _, tag := range ev.Tags {
			if len(tag) >= 2 && tag[0] == "r" {
				wssurl := strings.TrimRight(strings.TrimSpace(tag[1]), "/")
				if strings.HasPrefix(wssurl, "ws") {
					wssurls = append(wssurls, wssurl)
				}
			} else if len(tag) >= 2 && tag[0] == "proxy" && tag[2] == "activitypub" {
				valid = false
			}
		}
	}
	if valid {
		for _, wssurl := range wssurls {
			result[wssurl]++
		}
	}
	return result
}

func main() {
	relays := []string{
		"wss://yabu.me",
		"wss://relay-jp.nostr.wirednet.jp",
		"wss://nostr.compile-error.net",
		"wss://cagliostr.compile-error.net",
		"wss://r.kojira.io",
		"wss://nostream.ocha.one",
		"wss://nrelay.c-stellar.net",
		"wss://relay.nostr.wirednet.jp",
		//"wss://nostr-relay.nonce.academy",
		//"wss://relay.damus.io",
		//"wss://relay.nostr.bg",
		//"wss://nos.lol",
	}

	result := count(relays)

	dbURL := os.Getenv("DATABASE_URL")
	db, err := sql.Open("postgres", dbURL)
	if err != nil {
		log.Fatal(err)
	}
	defer db.Close()

	_, err = db.Exec(`
		CREATE TABLE IF NOT EXISTS relay_stats (
			id SERIAL PRIMARY KEY,
			date DATE NOT NULL,
			relay_url TEXT NOT NULL,
			subscription_count INTEGER NOT NULL,
			UNIQUE(date, relay_url)
		)
	`)
	if err != nil {
		log.Fatal(err)
	}

	today := time.Now().Format("2006-01-02")
	db.Exec("DELETE FROM relay_stats WHERE date = $1", today)

	tx, _ := db.Begin()
	stmt, _ := tx.Prepare("INSERT INTO relay_stats(date, relay_url, subscription_count) VALUES($1, $2, $3)")
	for url, cnt := range result {
		stmt.Exec(today, url, cnt)
	}
	tx.Commit()

	var ranks []Rank
	for url, cnt := range result {
		if cnt >= 20 {
			ranks = append(ranks, Rank{Name: url, Count: cnt})
		}
	}
	sort.Slice(ranks, func(i, j int) bool { return ranks[i].Count > ranks[j].Count })

	var wg sync.WaitGroup
	var mu sync.Mutex
	for i := range ranks {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			info := fetchRelayInfo(ranks[idx].Name)
			mu.Lock()
			ranks[idx].Description = info.Description
			mu.Unlock()
		}(i)
	}
	wg.Wait()

	line := charts.NewLine()
	line.SetGlobalOptions(
		charts.WithTitleOpts(opts.Title{
			Title: "Nostr Relay 利用者数推移（上位30）",
			TitleStyle: &opts.TextStyle{
				Color:      "#4f46e5",
				FontSize:   24,
				FontWeight: "bold",
			},
			Left: "center",
		}),
		charts.WithInitializationOpts(opts.Initialization{
			Theme:  types.ThemeMacarons,
			Width:  "100%",
			Height: "700px",
		}),
		charts.WithTooltipOpts(opts.Tooltip{Show: opts.Bool(true), Trigger: "axis"}),
		charts.WithLegendOpts(opts.Legend{
			Show:   opts.Bool(true),
			Orient: "horizontal",
			Bottom: "5%",
		}),
		charts.WithGridOpts(opts.Grid{
			Left:         "3%",
			Right:        "4%",
			Bottom:       "35%",
			Top:          "10%",
			ContainLabel: opts.Bool(true),
		}),
	)

	dates := make([]string, 20)
	base := time.Now().AddDate(0, 0, -19)
	for i := 0; i < 20; i++ {
		dates[i] = base.AddDate(0, 0, i).Format("01/02")
	}
	line.SetXAxis(dates)

	limit := 30
	if len(ranks) < limit {
		limit = len(ranks)
	}
	for _, r := range ranks[:limit] {
		var series []opts.LineData
		for i := 0; i < 20; i++ {
			queryDate := base.AddDate(0, 0, i).Format("2006-01-02")
			var cnt int
			err := db.QueryRow("SELECT subscription_count FROM relay_stats WHERE relay_url = $1 AND date = $2", r.Name, queryDate).Scan(&cnt)
			if err != nil {
				series = append(series, opts.LineData{})
			} else {
				series = append(series, opts.LineData{Value: cnt})
			}
		}
		short := strings.TrimPrefix(r.Name, "wss://")
		if len(short) > 30 {
			short = short[:27] + "..."
		}
		line.AddSeries(fmt.Sprintf("%s (%d)", short, r.Count), series,
			charts.WithLineChartOpts(opts.LineChart{
				Smooth:       opts.Bool(true),
				ShowSymbol:   opts.Bool(false),
				ConnectNulls: opts.Bool(true),
			}))
	}

	outputPath := os.Getenv("OUTPUT_PATH")
	if outputPath == "" {
		outputPath = "index.html"
	}
	f, err := os.Create(outputPath)
	if err != nil {
		log.Fatal(err)
	}
	defer f.Close()

	data := pageData{
		UpdateTime: time.Now().Format("2006年01月02日 15:04"),
		Ranks:      ranks,
	}

	renderer := &myRenderer{chart: line, data: data}
	if err := renderer.Render(f); err != nil {
		log.Fatal(err)
	}

	log.Println("✨ index.html が美しく生成されました！")
}
