package Protocol

import (
	"bytes"
	"context"
	"crypto/md5"
	"crypto/tls"
	"encoding/hex"
	"fmt"
	"github.com/PuerkitoBio/goquery"
	"github.com/fatih/color"
	"io"
	"net/http"
	"os"
	"regexp"
	"rscan/global"
	"rscan/poc"
	"strings"
	"time"
)

type WebInfo struct {
	URL         string   //url
	title       string   //标题
	statuscode  int      //状态码
	ContentType string   //类型
	app         string   //组件
	icohash     string   //server
	cert        certInfo //ssl证书信息
}

// certInfo 证书信息
type certInfo struct {
	certIssuer string //颁发者
	certDay    int    //过期天数
	signature  string //加密算法
	version    string //tls版本
}

func IsWeb(host, port string, timeout int, Poc bool) map[string]string {
	results := make(map[string]string)
	transport := &http.Transport{
		TLSClientConfig: &tls.Config{
			InsecureSkipVerify: true, //跳过证书的验证
		},
		DisableKeepAlives: true, //禁用HTTP连接的keep-alive 特性
	}

	client := &http.Client{
		Transport: transport,
		Timeout:   time.Duration(timeout) * time.Second,
	}

	for _, v := range []string{"https", "http"} {
		info := WebInfo{}
		switch port {
		case "443":
			info.URL = fmt.Sprintf("https://%s", host)
		case "80":
			info.URL = fmt.Sprintf("http://%s", host)
		default:
			info.URL = fmt.Sprintf("%s://%s:%s", v, host, port)
		}

		info.URL += global.WebURl //拼接扫描url后缀

		body, err := handleRequest(client, &info)
		if err != nil {
			continue
		}

		// 保存截图
		if global.SaveIMG {
			go func() {
				imgerr := global.CaptureScreenshot(info.URL, 90, global.SsaveIMGDIR)
				if imgerr != nil && strings.Contains(imgerr.Error(), "PATH") {
					fmt.Printf("\033[2K\r%s\n", "[ERROR] 在系统变量中不存在Chrom浏览器,跳过WEB截图功能!")
					global.SaveIMG = false
				}
			}()
		}

		// 验证漏洞，只允许运行30秒
		if Poc {
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			handlePocAndXss(ctx, &info, body)
		}

		// 识别组件
		results[v] = chekwebinfo(info)
		return results
	}
	return results
}

// handleRequest 请求网页并补充WebInfo结构体
func handleRequest(client *http.Client, info *WebInfo) ([]byte, error) {
	// 创建一个新的GET请求
	req, err := http.NewRequest("GET", info.URL, nil)

	if err != nil {
		return nil, err
	}

	// 设置请求头部信息，包括Cookie
	req.Header.Set("Cookie", "rememberMe=cFK")

	// 使用客户端执行请求
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)

	doc, err := goquery.NewDocumentFromReader(bytes.NewReader(body)) //获取标题
	if err == nil {
		info.title = strings.TrimSpace(doc.Find("title").Text())
	}
	//SSL信息
	if resp.TLS != nil {
		state := resp.TLS
		if len(state.PeerCertificates) > 0 {
			//过期天数
			remainingDays := int(state.PeerCertificates[0].NotAfter.Sub(time.Now()).Hours() / 24)
			info.cert.certDay = remainingDays
			//签发
			issuerCert := state.PeerCertificates[0].Issuer
			info.cert.certIssuer = strings.Split(issuerCert.CommonName, " ")[0]
			//加密算法
			signatureAlgorithm := state.PeerCertificates[0].SignatureAlgorithm.String()
			info.cert.signature = signatureAlgorithm
			// 判断协议版本
			switch state.Version {
			case tls.VersionTLS13:
				info.cert.version = "TLS1.3"
			case tls.VersionTLS12:
				info.cert.version = "TLS1.2"
			case tls.VersionTLS11:
				info.cert.version = "TLS1.1"
			case tls.VersionTLS10:
				info.cert.version = "TLS1.0"
			}
		}
	}

	info.statuscode = resp.StatusCode
	info.ContentType = resp.Header.Get("Content-Type")
	info.icohash, _ = HashMD5(info.URL)
	info.app = CheckApp(string(body), resp.Header, resp.Cookies(), info.icohash) // 匹配组件

	if os.Getenv("html") == "on" {
		//fmt.Printf("-----> URL: %s HTML正文:\n%s\n", info.URL, string(body))
		//fmt.Printf("-----> Header:\n")
		for k, v := range resp.Header {
			fmt.Println(k, "->", v)
		}
	}

	return body, nil
}

// handlePocAndXss 漏洞扫描以及POC扫描
func handlePocAndXss(ctx context.Context, info *WebInfo, body []byte) {
	done := make(chan bool, 1)

	go func() {
		defer func() {
			done <- true

		}()
		poc.CheckPoc(info.URL, info.app) //POC扫描

		// 基于title确认是否url是目录浏览
		if strings.Contains(strings.ToLower(info.title), "index of") {
			poc.ListPocInfo = append(poc.ListPocInfo, poc.Flagcve{Url: info.URL, Cve: "目录浏览漏洞"})
		}

	}()
	select {
	case <-done:
	case <-ctx.Done(): //超时退出
	}
}

// CheckApp 基于返回的body、headers、cookies判定组件信息
func CheckApp(body string, head map[string][]string, cookies []*http.Cookie, icohash string) string {

	var app []string
	for _, rule := range RuleDatas {
		switch rule.Type {
		case "body":
			patterns, err := regexp.Compile(rule.Rule)
			if err == nil && patterns.MatchString(body) {
				app = append(app, rule.Name)
			}

		case "headers":
			for k, values := range head {
				for _, value := range values {
					patterns, err := regexp.Compile(`(?i)` + rule.Rule) //不区分大小写
					if err == nil && patterns.MatchString(value) || patterns.MatchString(k) {
						app = append(app, rule.Name)
					}
				}
			}

		case "cookie":
			for _, cookie := range cookies {
				patterns, err := regexp.Compile(`(?i)` + rule.Rule) //不区分大小写
				if err == nil && patterns.MatchString(cookie.Name) {
					app = append(app, rule.Name)
				}
			}

		case "icohash":
			if strings.EqualFold(rule.Rule, icohash) {
				app = append(app, rule.Name)
			}
		}
	}

	return strings.Join(global.RemoveDuplicates(app), ",")

}

func HashMD5(url string) (string, error) {
	transport := &http.Transport{
		TLSClientConfig: &tls.Config{
			InsecureSkipVerify: true, //跳过证书的验证
		},
		DisableKeepAlives: true, //禁用HTTP连接的keep-alive 特性
	}

	client := &http.Client{
		Transport: transport,
	}
	req, err := http.NewRequest("GET", url+"/favicon.ico", nil)
	if err != nil {
		return "", err
	}
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close() // 确保响应体在函数结束时关闭

	// 创建一个新的 MD5 哈希对象
	hash := md5.New()
	_, err = io.Copy(hash, resp.Body)
	if err != nil {
		fmt.Println("Error:", err)
		return "", err
	}
	md5Hex := hex.EncodeToString(hash.Sum(nil))
	return md5Hex, nil
}

func chekwebinfo(info WebInfo) string {
	output := fmt.Sprintf("%-20s ", info.URL)

	if info.app != "" {
		output += color.GreenString("%s", fmt.Sprintf("「%s」", info.app))
	}

	if info.title != "" {
		info.title = strings.ReplaceAll(info.title, "  ", "")
		output += color.BlueString("%s", fmt.Sprintf(" title:「%s」", info.title))
	}

	/*	if info.cert.version != "" && info.cert.certIssuer != "" && info.cert.certDay != 0 && info.cert.signature != "" {
		output += color.CyanString("%s", fmt.Sprintf("「%d %s %s %s」", info.cert.certDay, info.cert.version, info.cert.signature, info.cert.certIssuer))
	}*/

	output += fmt.Sprintf("「%d」", info.statuscode)

	/*	if info.ContentType != "" {
		output += fmt.Sprintf("「%s」", info.ContentType)
	}*/

	return output
}
