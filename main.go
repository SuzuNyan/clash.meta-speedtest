package main

import (
	"context"
	"encoding/csv"
	"flag"
	"fmt"
	"io"
	"math"
	"net"
	"net/http"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Dreamacro/clash/adapter"
	"github.com/Dreamacro/clash/adapter/provider"
	C "github.com/Dreamacro/clash/constant"
	"github.com/Dreamacro/clash/log"

	"gopkg.in/yaml.v3" // replaced by "github.com/braydonk/yaml", check go.mod for details
)

var (
	livenessObject     = flag.String("l", "https://speed.cloudflare.com/__down?bytes=%d", "liveness object, support http(s) url, support payload too")
	configPathConfig   = flag.String("c", "", "configuration file path, also support http(s) url")
	filterRegexConfig  = flag.String("f", ".*", "filter proxies by name, use regexp")
	downloadSizeConfig = flag.Int("size", 1024*1024*10, "download size for testing proxies")
	timeoutConfig      = flag.Duration("timeout", time.Second*5, "timeout for testing proxies")
	sortField          = flag.String("sort", "b", "sort field for testing proxies, b for bandwidth, t for TTFB")
	output             = flag.String("output", "", "output result to csv/yaml file")
	concurrent         = flag.Int("concurrent", 1, "download concurrent size")
	speed              = flag.Float64("speed", 1024, "target download speed (KB/s)")
	speedCheckInterval = flag.Duration("speed-check-interval", time.Millisecond*100, "download speed check interval")
	parrallel          = flag.Int("parrallel", 4, "parrallel test nodes")
	downloadTime       = flag.Float64("downloadTime", 10.0, "Download for serveral FTTB")
)

type CProxy struct {
	C.Proxy
	RawNode yaml.Node
}

type Result struct {
	Name         string
	Bandwidth    float64
	TTFB         time.Duration
	MaxSpeed     float64
	DownloadTime time.Duration
}

var (
	red   = "\033[31m"
	green = "\033[32m"
)

type RawConfig struct {
	Providers map[string]map[string]any `yaml:"proxy-providers"`
	Proxies   []yaml.Node               `yaml:"proxies"`
}

func main() {
	flag.Parse()

	if *configPathConfig == "" {
		log.Fatalln("Please specify the configuration file")
	}

	var allProxies = make(map[string]CProxy)
	for _, configPath := range strings.Split(*configPathConfig, ",") {
		var body []byte
		var err error
		if strings.HasPrefix(configPath, "http") {
			var resp *http.Response
			resp, err = http.Get(configPath)
			if err != nil {
				log.Warnln("failed to fetch config: %s", err)
				continue
			}
			body, err = io.ReadAll(resp.Body)
		} else {
			body, err = os.ReadFile(configPath)
		}
		if err != nil {
			log.Warnln("failed to read config: %s", err)
			continue
		}

		lps, err := loadProxies(body)
		if err != nil {
			log.Fatalln("Failed to convert : %s", err)
		}

		for k, p := range lps {
			if _, ok := allProxies[k]; !ok {
				allProxies[k] = p
			}
		}
	}

	filteredProxies := filterProxies(*filterRegexConfig, allProxies)

	format := "%s%-22s\t%-12s\t%-12s\t%-12s\t%-12s\033[0m\n"

	fmt.Printf(format, "", "节点", "带宽", "Max速度", "延迟", "下载用时")
	results := TestProxiesParrallel(filteredProxies, allProxies, format)
	lastResults := make([]*Result, 0, len(results))
		for _, result := range results{
			if result.Bandwidth > 0{
				lastResults = append(lastResults, result)
			}
		}
	if *sortField != "" {
		switch *sortField {
		case "b", "bandwidth":
			sort.Slice(results, func(i, j int) bool {
				return results[i].Bandwidth > results[j].Bandwidth
			})
			fmt.Println("\n\n===结果按照带宽排序===")
		case "t", "ttfb":
			sort.Slice(results, func(i, j int) bool {
				return results[i].TTFB < results[j].TTFB
			})
			fmt.Println("\n\n===结果按照延迟排序===")
		case "m", "maxSpeed":
			sort.Slice(results, func(i, j int) bool {
				return results[i].MaxSpeed < results[j].MaxSpeed
			})
			fmt.Println("\n\n===结果按照单个连接测得的最大速度排序===")
		default:
			log.Fatalln("Unsupported sort field: %s", *sortField)
		}
		fmt.Printf(format, "", "节点", "带宽", "Max速度", "延迟", "下载用时")
		
		for _, result := range lastResults {
			result.Printf(format)
		}
	}

	if strings.EqualFold(*output, "yaml") {
		if err := writeNodeConfigurationToYAML("result.yaml", lastResults, allProxies); err != nil {
			log.Fatalln("Failed to write yaml: %s", err)
		}
	} else if strings.EqualFold(*output, "csv") {
		if err := writeToCSV("result.csv", lastResults); err != nil {
			log.Fatalln("Failed to write csv: %s", err)
		}
	}
}

func TestProxiesParrallel(filteredProxies []string, allProxies map[string]CProxy, format string) []*Result {
	results := make([]*Result, 0, len(filteredProxies))
	var actualParrallel int
	if *parrallel < 1 {
		actualParrallel = 1
	}else{
		actualParrallel = *parrallel
	}
	ch := make(chan *Result, actualParrallel)
	firstBundleSize := actualParrallel
	if firstBundleSize > len(filteredProxies) {
		firstBundleSize = len(filteredProxies)
	}
	runCounter := 0
	runningCounter := 0
	for n := 0; n < firstBundleSize; n++ {
		go goproxy(ch, filteredProxies[n], allProxies[filteredProxies[n]])
	}
	runningCounter = firstBundleSize

	for runCounter < len(filteredProxies) {
		result := <-ch
		results = append(results, result)
		if result != nil {
			result.Printf(format)
		}
		runCounter++
		runningCounter--
		if runningCounter+runCounter < len(filteredProxies) && runningCounter < *parrallel {
			go goproxy(ch, filteredProxies[runCounter+runningCounter], allProxies[filteredProxies[runCounter+runningCounter]])
			runningCounter++
		}

	}

	return results
}

func goproxy(ch chan *Result, name string, proxy CProxy) {
	switch proxy.Type() {
	case C.Shadowsocks, C.ShadowsocksR, C.Snell, C.Socks5, C.Http, C.Vmess, C.Vless, C.Trojan, C.Hysteria, C.WireGuard, C.Tuic:
		ch <- TestProxyConcurrent(name, proxy, *downloadSizeConfig, *timeoutConfig, *concurrent)
	case C.Direct, C.Reject, C.Pass, C.Relay, C.Selector, C.Fallback, C.URLTest, C.LoadBalance:
		ch <- nil
	default:
		log.Fatalln("Unsupported proxy type: %s", proxy.Type())
		ch <- nil
	}

}

func filterProxies(filter string, proxies map[string]CProxy) []string {
	filterRegexp := regexp.MustCompile(filter)
	filteredProxies := make([]string, 0, len(proxies))
	for name := range proxies {
		if filterRegexp.MatchString(name) {
			filteredProxies = append(filteredProxies, name)
		}
	}
	sort.Strings(filteredProxies)
	return filteredProxies
}

func loadProxies(buf []byte) (map[string]CProxy, error) {
	rawCfg := &RawConfig{
		Proxies: []yaml.Node{},
	}
	if err := yaml.Unmarshal(buf, rawCfg); err != nil {
		return nil, err
	}
	proxies := make(map[string]CProxy)
	proxiesConfig := rawCfg.Proxies
	providersConfig := rawCfg.Providers

	for i, node := range proxiesConfig {
		decoded := make(map[string]any)
		err := node.Decode(decoded)
		if err != nil {
			return nil, fmt.Errorf("proxy %d: %w", i, err)
		}
		proxy, err := adapter.ParseProxy(decoded)
		if err != nil {
			return nil, fmt.Errorf("proxy %d: %w", i, err)
		}

		if _, exist := proxies[proxy.Name()]; exist {
			return nil, fmt.Errorf("proxy %s is the duplicate name", proxy.Name())
		}
		proxies[proxy.Name()] = CProxy{Proxy: proxy, RawNode: node}
	}
	for name, config := range providersConfig {
		if name == provider.ReservedName {
			return nil, fmt.Errorf("can not defined a provider called `%s`", provider.ReservedName)
		}
		pd, err := provider.ParseProxyProvider(name, config)
		if err != nil {
			return nil, fmt.Errorf("parse proxy provider %s error: %w", name, err)
		}
		if err := pd.Initial(); err != nil {
			return nil, fmt.Errorf("initial proxy provider %s error: %w", pd.Name(), err)
		}
		for _, proxy := range pd.Proxies() {
			proxies[fmt.Sprintf("[%s] %s", name, proxy.Name())] = CProxy{Proxy: proxy}
		}
	}
	return proxies, nil
}

func (r *Result) Printf(format string) {
	color := ""
	if r.Bandwidth < 1024*1024 {
		color = red
	} else if r.Bandwidth > *speed {
		color = green
	}
	fmt.Printf(format, color, formatName(r.Name), formatBandwidth(r.Bandwidth), formatBandwidth(r.MaxSpeed), formatMilliseconds(r.TTFB), formatMilliseconds(r.DownloadTime))
}

func TestProxyConcurrent(name string, proxy C.Proxy, downloadSize int, timeout time.Duration, concurrentCount int) *Result {
	if concurrentCount <= 0 {
		concurrentCount = 1
	}

	chunkSize := downloadSize / concurrentCount
	totalTTFB := int64(0)
	totalDownloadTime := int64(0)
	downloaded := int64(0)
	var maxSpeed atomic.Value
	maxSpeed.Store(float64(0))

	var wg sync.WaitGroup

	// start := time.Now()
	for i := 0; i < concurrentCount; i++ {
		wg.Add(1)
		go func(i int) {
			result, w := TestProxy(name, proxy, chunkSize, timeout)
			if w != 0 {
				atomic.AddInt64(&downloaded, w)
				atomic.AddInt64(&totalTTFB, int64(result.TTFB))
				atomic.AddInt64(&totalDownloadTime, int64(result.DownloadTime))
				currentMax := maxSpeed.Load().(float64)
				for currentMax < result.MaxSpeed {
					if maxSpeed.CompareAndSwap(currentMax, result.MaxSpeed) {
						break
					} else {
						currentMax = maxSpeed.Load().(float64)
						continue
					}
				}
			}
			wg.Done()
		}(i)
	}
	wg.Wait()
	downloadTime := time.Duration(totalDownloadTime / int64(concurrentCount))
	bandwidth := float64(downloaded) / downloadTime.Seconds()
	if math.IsNaN(bandwidth) {
		bandwidth = -1
	}
	result := &Result{
		Name:         name,
		Bandwidth:    bandwidth,
		TTFB:         time.Duration(totalTTFB / int64(concurrentCount)),
		MaxSpeed:     maxSpeed.Load().(float64),
		DownloadTime: downloadTime,
	}
	return result
}

func TestProxy(name string, proxy C.Proxy, downloadSize int, timeout time.Duration) (*Result, int64) {
	client := http.Client{
		Timeout: timeout,
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
				host, port, err := net.SplitHostPort(addr)
				if err != nil {
					return nil, err
				}
				var uint16Port uint16
				if port, err := strconv.ParseUint(port, 10, 16); err != nil {
					return nil, err
				} else {
					uint16Port = uint16(port)
				}
				return proxy.DialContext(ctx, &C.Metadata{
					Host:    host,
					DstPort: uint16Port,
				})
			},
		},
	}

	start := time.Now()
	resp, err := client.Get(fmt.Sprintf(*livenessObject, downloadSize))
	if err != nil {
		return &Result{name, -1, -1, -1, -1}, 0
	}
	defer resp.Body.Close()
	if resp.StatusCode-http.StatusOK > 100 {
		return &Result{name, -1, -1, -1, -1}, 0
	}
	ttfb := time.Since(start)
	checkTimer := time.NewTicker(*speedCheckInterval)
	closeTimer := time.NewTimer(time.Duration(float64(ttfb) * *downloadTime))

	defer checkTimer.Stop()
	defer closeTimer.Stop()

	buffer := make([]byte, math.MaxUint16)
	lastTick := time.Now()
	maxSpeed := 0.0
	totalBytesRead := int64(0)
	prevBytesRead := int64(0)
LOOP:
	for {
		select {
		case current := <-checkTimer.C:
			elapsedTime := time.Since(lastTick).Seconds()
			downloadSpeed := float64(totalBytesRead-prevBytesRead) / elapsedTime
			if downloadSpeed > maxSpeed {
				maxSpeed = downloadSpeed
				if *speed > 0 && maxSpeed > *speed*1024 {
					break LOOP
				}
			}
			lastTick = current
			prevBytesRead = totalBytesRead
		case <-closeTimer.C:
			resp.Body.Close()
			break LOOP
		default:
			n, err := resp.Body.Read(buffer)
			if err == io.EOF {
				// fmt.Println("Download completed.")
				break LOOP
			}
			if err != nil {
				fmt.Println("Error:", err)
				return &Result{name, -1, -1, -1, -1}, 0
			}
			totalBytesRead += int64(n)
		}
	}
	downloadTime := time.Since(start) - ttfb
	bandwidth := float64(totalBytesRead) / downloadTime.Seconds()
	return &Result{name, bandwidth, ttfb, maxSpeed, downloadTime}, totalBytesRead
}

var (
	emojiRegex = regexp.MustCompile(`[\x{1F600}-\x{1F64F}\x{1F300}-\x{1F5FF}\x{1F680}-\x{1F6FF}\x{2600}-\x{26FF}\x{1F1E0}-\x{1F1FF}]`)
	spaceRegex = regexp.MustCompile(`\s{2,}`)
)

func formatName(name string) string {
	noEmoji := emojiRegex.ReplaceAllString(name, "")
	mergedSpaces := spaceRegex.ReplaceAllString(noEmoji, " ")
	return strings.TrimSpace(mergedSpaces)
}

func formatBandwidth(v float64) string {
	if v <= 0 {
		return "N/A"
	}
	if v < 1024 {
		return fmt.Sprintf("%.02fB/s", v)
	}
	v /= 1024
	if v < 1024 {
		return fmt.Sprintf("%.02fKB/s", v)
	}
	v /= 1024
	if v < 1024 {
		return fmt.Sprintf("%.02fMB/s", v)
	}
	v /= 1024
	if v < 1024 {
		return fmt.Sprintf("%.02fGB/s", v)
	}
	v /= 1024
	return fmt.Sprintf("%.02fTB/s", v)
}

func formatMilliseconds(v time.Duration) string {
	if v <= 0 {
		return "N/A"
	}
	return fmt.Sprintf("%.02fms", float64(v.Milliseconds()))
}

func writeNodeConfigurationToYAML(filePath string, results []*Result, proxies map[string]CProxy) error {
	fp, err := os.Create(filePath)
	if err != nil {
		return err
	}
	defer fp.Close()

	var sortedProxies []yaml.Node
	for _, result := range results {
		if v, ok := proxies[result.Name]; ok {
			sortedProxies = append(sortedProxies, v.RawNode)
		}
	}

	bytes, err := yaml.Marshal(sortedProxies)
	if err != nil {
		return err
	}

	_, err = fp.Write(bytes)
	return err
}

func writeToCSV(filePath string, results []*Result) error {
	csvFile, err := os.Create(filePath)
	if err != nil {
		return err
	}
	defer csvFile.Close()

	// 写入 UTF-8 BOM 头
	csvFile.WriteString("\xEF\xBB\xBF")

	csvWriter := csv.NewWriter(csvFile)
	err = csvWriter.Write([]string{"节点", "带宽 (MB/s)", "最大速度", "延迟 (ms)"})
	if err != nil {
		return err
	}
	for _, result := range results {
		line := []string{
			result.Name,
			fmt.Sprintf("%.2f", result.Bandwidth/1024/1024),
			fmt.Sprintf("%.2f", result.MaxSpeed/1024/1024),
			strconv.FormatInt(result.TTFB.Milliseconds(), 10),
		}
		err = csvWriter.Write(line)
		if err != nil {
			return err
		}
	}
	csvWriter.Flush()
	return nil
}
