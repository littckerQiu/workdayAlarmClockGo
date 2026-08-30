/*
 * 米家 API 客户端
 * 参考 MiWu (https://github.com/sky130/MiWu) 的 Kotlin 实现移植到 Go
 * 支持：账号密码登录、获取家庭列表、获取设备列表、查询设备属性（开关状态）
 * zyyme 20260828
 */
package mihome

import (
	"crypto/hmac"
	"crypto/md5"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"math/rand"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"strings"
	"time"

	"github.com/zanjie1999/httpme"
)

const (
	miotServerURL    = "https://api.io.mi.com/app/"
	miotUserAgent    = "APP/com.xiaomi.mihome APPV/6.0.103 iosPassportSDK/3.9.0 iOS/14.4 miHSTS"
	miotSid          = "xiaomiio"
	serviceLoginURL  = "https://account.xiaomi.com/pass/serviceLogin?sid=" + miotSid + "&_json=true"
	serviceLoginAuth = "https://account.xiaomi.com/pass/serviceLoginAuth2"
	qrcodeGenerateURL = "https://account.xiaomi.com/longPolling/loginUrl"
)

// QrCodeInfo 扫码登录二维码信息
type QrCodeInfo struct {
	QrData   string `json:"qrData"`   // 二维码内容（用米家App扫描）
	PollUrl  string `json:"pollUrl"`  // 长轮询URL
}

// User 米家用户凭证
type User struct {
	UserID       string `json:"userId"`
	CUserID      string `json:"cUserId"`
	Nonce        int64  `json:"nonce"`
	SSecurity    string `json:"ssecurity"`
	PSecurity    string `json:"psecurity"`
	PassToken    string `json:"passToken"`
	ServiceToken string `json:"serviceToken"`
	DeviceID     string `json:"deviceId"`
}

// Home 米家家庭
type Home struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	UID  int64  `json:"uid"`
}

type homeListResult struct {
	HasMore       bool   `json:"has_more"`
	HomeList      []Home `json:"homelist"`
	ShareHomeList []Home `json:"share_home_list"`
	MaxID         string `json:"max_id"`
}

type miotResponse struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Result  json.RawMessage `json:"result"`
}

// Device 米家设备
type Device struct {
	Did      string `json:"did"`
	Name     string `json:"name"`
	Model    string `json:"model"`
	IsOnline bool   `json:"isOnline"`
	Token    string `json:"token"`
	SpecType string `json:"spec_type"`
}

type deviceListResult struct {
	DeviceInfo []Device `json:"device_info"`
	HasMore    bool     `json:"has_more"`
	MaxDid     string   `json:"max_did"`
}

// Property 设备属性
type Property struct {
	Did    string      `json:"did"`
	Siid   int         `json:"siid"`
	Piid   int         `json:"piid"`
	Value  interface{} `json:"value"`
	Code   int         `json:"code"`
	Iid    string      `json:"iid"`
	ExeTime int        `json:"exe_time"`
}

var (
	currentUser *User
	loginJar, _ = cookiejar.New(nil)
	httpClient  = &http.Client{Timeout: 15 * time.Second, Jar: loginJar}
)

func init() {
	httpme.SetSkipVerify(true)
}

// IsLoggedIn 是否已登录
func IsLoggedIn() bool {
	return currentUser != nil && currentUser.ServiceToken != "" && currentUser.SSecurity != ""
}

// SetUser 直接设置用户凭证（从配置加载）
func SetUser(u *User) {
	currentUser = u
}

// GetUser 获取当前用户凭证（用于保存）
func GetUser() *User {
	return currentUser
}

// Logout 注销米家登录
func Logout() {
	currentUser = nil
}

// Login 用账号密码登录米家
func Login(username, password string) (*User, error) {
	// 1. 获取登录位置信息
	loc, sign, qs, callback, sid, err := getLoginLocation()
	if err != nil {
		return nil, fmt.Errorf("获取登录位置失败: %w", err)
	}

	// 2. MD5加密密码（大写）
	pwdMd5 := strings.ToUpper(fmt.Sprintf("%x", md5.Sum([]byte(password))))

	// 3. POST 登录认证
	form := url.Values{}
	form.Set("qs", qs)
	form.Set("sid", sid)
	form.Set("_sign", sign)
	form.Set("callback", callback)
	form.Set("user", username)
	form.Set("hash", pwdMd5)
	form.Set("_json", "true")

	req, err := http.NewRequest("POST", serviceLoginAuth, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", miotUserAgent)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("登录请求失败: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}

	cleaned := stripJSONP(string(body))
	var authResp struct {
		Code      int    `json:"code"`
		Desc      string `json:"desc"`
		Location  string `json:"location"`
		SSecurity string `json:"ssecurity"`
		UserID    int64  `json:"userId"`
		CUserID   string `json:"cUserId"`
		PassToken string `json:"passToken"`
		Nonce     int64  `json:"nonce"`
		PSecurity string `json:"psecurity"`
	}
	if err := json.Unmarshal([]byte(cleaned), &authResp); err != nil {
		return nil, fmt.Errorf("解析登录响应失败: %w, body: %s", err, cleaned)
	}
	if authResp.Code != 0 {
		return nil, fmt.Errorf("登录失败 code=%d desc=%s", authResp.Code, authResp.Desc)
	}
	_ = loc

	// 4. 访问 location 获取 serviceToken
	serviceToken, err := getServiceToken(authResp.Location)
	if err != nil {
		return nil, fmt.Errorf("获取serviceToken失败: %w", err)
	}

	currentUser = &User{
		UserID:       fmt.Sprintf("%d", authResp.UserID),
		CUserID:      authResp.CUserID,
		Nonce:        authResp.Nonce,
		SSecurity:    authResp.SSecurity,
		PSecurity:    authResp.PSecurity,
		PassToken:    authResp.PassToken,
		ServiceToken: serviceToken,
		DeviceID:     RandomDeviceID(),
	}
	log.Println("米家登录成功，用户ID:", currentUser.UserID)
	return currentUser, nil
}

func getLoginLocation() (location, sign, qs, callback, sid string, err error) {
	req, err := http.NewRequest("GET", serviceLoginURL, nil)
	if err != nil {
		return
	}
	req.Header.Set("User-Agent", miotUserAgent)
	resp, err := httpClient.Do(req)
	if err != nil {
		return
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return
	}
	cleaned := stripJSONP(string(body))
	var data struct {
		Sign     string `json:"_sign"`
		QS       string `json:"qs"`
		Callback string `json:"callback"`
		Sid      string `json:"sid"`
		Location string `json:"location"`
	}
	if err = json.Unmarshal([]byte(cleaned), &data); err != nil {
		err = fmt.Errorf("解析登录位置失败: %w, body: %s", err, cleaned)
		return
	}
	return data.Location, data.Sign, data.QS, data.Callback, data.Sid, nil
}

func getServiceToken(location string) (string, error) {
	req, err := http.NewRequest("GET", location, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("User-Agent", miotUserAgent)
	resp, err := httpClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	// 1. 从响应的 Set-Cookie 中提取（可能有多个）
	for _, sc := range resp.Header.Values("Set-Cookie") {
		// Set-Cookie 格式: name=value; Path=...; ...
		firstPart := strings.SplitN(sc, ";", 2)[0]
		if strings.HasPrefix(firstPart, "serviceToken=") {
			return strings.TrimPrefix(firstPart, "serviceToken="), nil
		}
	}
	// 2. 从 resp.Cookies() 中提取
	for _, cookie := range resp.Cookies() {
		if cookie.Name == "serviceToken" {
			return cookie.Value, nil
		}
	}
	// 3. 从 Cookie Jar 中提取（重定向时 Set-Cookie 可能只存在于 jar 中）
	if u, err := url.Parse(location); err == nil {
		for _, cookie := range loginJar.Cookies(u) {
			if cookie.Name == "serviceToken" {
				return cookie.Value, nil
			}
		}
	}
	// 4. 也检查 account.xiaomi.com 的 cookie（可能重定向到了不同域名）
	if u, err := url.Parse("https://account.xiaomi.com"); err == nil {
		for _, cookie := range loginJar.Cookies(u) {
			if cookie.Name == "serviceToken" {
				return cookie.Value, nil
			}
		}
	}
	return "", fmt.Errorf("Set-Cookie中未找到serviceToken")
}

// GenerateQrCode 生成扫码登录二维码
// 返回二维码内容和长轮询URL
func GenerateQrCode() (*QrCodeInfo, error) {
	qURL := fmt.Sprintf("%s?_qrsize=240&qs=?sid=%s&callback=https://sts.api.io.mi.com/sts&sid=%s&serviceParam=&_locale=zh_CN&_dc=%d",
		qrcodeGenerateURL, miotSid, miotSid, time.Now().UnixMilli())

	req, err := http.NewRequest("GET", qURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", miotUserAgent)
	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	cleaned := stripJSONP(string(body))
	var qr struct {
		Code     int    `json:"code"`
		Desc     string `json:"desc"`
		LoginURL string `json:"loginUrl"`
		Lp       string `json:"lp"`
		Qr       string `json:"qr"`
	}
	if err := json.Unmarshal([]byte(cleaned), &qr); err != nil {
		return nil, fmt.Errorf("解析二维码响应失败: %w, body: %s", err, cleaned)
	}
	if qr.LoginURL == "" || qr.Lp == "" {
		return nil, fmt.Errorf("获取二维码失败: %s", qr.Desc)
	}
	// qr.LoginURL 是二维码内容，qr.Lp 是长轮询URL
	qrData := qr.LoginURL
	if qr.Qr != "" && !strings.HasPrefix(qr.Qr, "http") {
		// 某些情况下 qr 字段包含实际的二维码内容
		qrData = qr.Qr
	}
	return &QrCodeInfo{
		QrData:  qrData,
		PollUrl: qr.Lp,
	}, nil
}

// QrLoginResult 扫码登录结果
type QrLoginResult struct {
	Success bool   `json:"success"`
	Waiting bool   `json:"waiting"` // 等待扫码
	Message string `json:"message"`
}

// PollQrLogin 长轮询扫码登录结果
// 此方法会阻塞直到用户扫码确认或超时（约5分钟）
func PollQrLogin(pollUrl string) (*User, error) {
	// 长轮询客户端，超时5分钟，共享Cookie Jar
	pollClient := &http.Client{Timeout: 320 * time.Second, Jar: loginJar}
	req, err := http.NewRequest("GET", pollUrl, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", miotUserAgent)
	resp, err := pollClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	cleaned := stripJSONP(string(body))
	var authResp struct {
		Code      int    `json:"code"`
		Desc      string `json:"desc"`
		Location  string `json:"location"`
		SSecurity string `json:"ssecurity"`
		UserID    int64  `json:"userId"`
		CUserID   string `json:"cUserId"`
		PassToken string `json:"passToken"`
		Nonce     int64  `json:"nonce"`
		PSecurity string `json:"psecurity"`
	}
	if err := json.Unmarshal([]byte(cleaned), &authResp); err != nil {
		return nil, fmt.Errorf("解析扫码登录响应失败: %w, body: %s", err, cleaned)
	}
	if authResp.Code != 0 {
		return nil, fmt.Errorf("QR_LOGIN_WAITING:%s", authResp.Desc)
	}

	// 扫码成功后，重新获取 serviceData（location 和 ssecurity）
	loc, ssecurity, err := getServiceData()
	if err != nil {
		// 如果获取失败，使用长轮询返回的值
		loc = authResp.Location
		ssecurity = authResp.SSecurity
		log.Println("获取serviceData失败，使用轮询返回值:", err)
	}

	// 获取 serviceToken
	serviceToken, err := getServiceToken(loc)
	if err != nil {
		return nil, fmt.Errorf("获取serviceToken失败: %w", err)
	}

	currentUser = &User{
		UserID:       fmt.Sprintf("%d", authResp.UserID),
		CUserID:      authResp.CUserID,
		Nonce:        authResp.Nonce,
		SSecurity:    ssecurity,
		PSecurity:    authResp.PSecurity,
		PassToken:    authResp.PassToken,
		ServiceToken: serviceToken,
		DeviceID:     RandomDeviceID(),
	}
	log.Println("米家扫码登录成功，用户ID:", currentUser.UserID)
	return currentUser, nil
}

// getServiceData 获取扫码登录后的 serviceData（location + ssecurity）
func getServiceData() (string, string, error) {
	req, err := http.NewRequest("GET", serviceLoginURL, nil)
	if err != nil {
		return "", "", err
	}
	req.Header.Set("User-Agent", miotUserAgent)
	resp, err := httpClient.Do(req)
	if err != nil {
		return "", "", err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", "", err
	}
	cleaned := stripJSONP(string(body))
	var data struct {
		Location  string `json:"location"`
		SSecurity string `json:"ssecurity"`
	}
	if err := json.Unmarshal([]byte(cleaned), &data); err != nil {
		return "", "", err
	}
	return data.Location, data.SSecurity, nil
}

// signedRequest 发送签名的米家 API 请求
// path 是 /app/ 之后的路径，例如 "/v2/homeroom/gethome"
func signedRequest(path string, body interface{}) (json.RawMessage, error) {
	if !IsLoggedIn() {
		return nil, fmt.Errorf("未登录米家")
	}

	dataBytes, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	data := string(dataBytes)

	nonce := randomNonce()
	signedNonce, err := genSignedNonce(currentUser.SSecurity, nonce)
	if err != nil {
		return nil, err
	}
	// 签名用的 URI 是 /app/ 之后的路径，带前导 /
	signURI := path
	signature, err := genSignature(signURI, signedNonce, nonce, data)
	if err != nil {
		return nil, err
	}

	form := url.Values{}
	form.Set("_nonce", nonce)
	form.Set("data", data)
	form.Set("signature", signature)

	fullURL := miotServerURL + strings.TrimPrefix(path, "/")
	req, err := http.NewRequest("POST", fullURL, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", miotUserAgent)
	req.Header.Set("x-xiaomi-protocal-flag-cli", "PROTOCAL-HTTP2")
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Cookie", fmt.Sprintf("PassportDeviceId=%s;userId=%s;serviceToken=%s",
		currentUser.DeviceID, currentUser.UserID, currentUser.ServiceToken))

	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}

	var mr miotResponse
	if err := json.Unmarshal(respBody, &mr); err != nil {
		return nil, fmt.Errorf("解析响应失败: %w, body: %s", err, string(respBody))
	}
	if mr.Code != 0 {
		// 登录失效
		if mr.Code == 401 || mr.Code == -1 {
			log.Println("米家登录凭证可能已失效 code:", mr.Code, mr.Message)
		}
		return nil, fmt.Errorf("米家API错误 code=%d message=%s", mr.Code, mr.Message)
	}
	return mr.Result, nil
}

// GetHomes 获取家庭列表
func GetHomes() ([]Home, error) {
	body := map[string]interface{}{
		"app_ver":         7,
		"fetch_share":     true,
		"fetch_share_dev": true,
		"fg":              false,
		"limit":           300,
	}
	result, err := signedRequest("/v2/homeroom/gethome", body)
	if err != nil {
		return nil, err
	}
	var hl homeListResult
	if err := json.Unmarshal(result, &hl); err != nil {
		return nil, err
	}
	homes := append(hl.HomeList, hl.ShareHomeList...)
	return homes, nil
}

// GetDevices 获取指定家庭下的设备列表
func GetDevices(homeID string, ownerUID int64) ([]Device, error) {
	body := map[string]interface{}{
		"home_owner": ownerUID,
		"home_id":    homeID,
		"limit":      200,
	}
	result, err := signedRequest("/v2/home/home_device_list", body)
	if err != nil {
		return nil, err
	}
	var dl deviceListResult
	if err := json.Unmarshal(result, &dl); err != nil {
		return nil, err
	}
	return dl.DeviceInfo, nil
}

// GetDeviceProp 获取设备属性
func GetDeviceProp(did string, siid, piid int) (*Property, error) {
	body := map[string]interface{}{
		"params": []map[string]interface{}{
			{"did": did, "siid": siid, "piid": piid},
		},
	}
	result, err := signedRequest("/miotspec/prop/get", body)
	if err != nil {
		return nil, err
	}
	var props []Property
	if err := json.Unmarshal(result, &props); err != nil {
		return nil, err
	}
	if len(props) == 0 {
		return nil, fmt.Errorf("设备 %s 未返回属性", did)
	}
	return &props[0], nil
}

// IsDeviceOn 查询设备是否打开（开关属性为布尔值）
func IsDeviceOn(did string, siid, piid int) (bool, error) {
	prop, err := GetDeviceProp(did, siid, piid)
	if err != nil {
		return false, err
	}
	if prop.Code != 0 {
		return false, fmt.Errorf("属性查询错误 code=%d", prop.Code)
	}
	switch v := prop.Value.(type) {
	case bool:
		return v, nil
	case float64:
		return v != 0, nil
	case string:
		return v == "true" || v == "1" || v == "on", nil
	default:
		return false, fmt.Errorf("未知的属性值类型: %T (%v)", v, v)
	}
}

// genSignedNonce 生成签名 nonce
// signedNonce = base64(sha256(base64decode(ssecurity) + base64decode(nonce)))
func genSignedNonce(ssecurity, nonce string) (string, error) {
	ss, err := base64.StdEncoding.DecodeString(ssecurity)
	if err != nil {
		return "", fmt.Errorf("ssecurity base64解码失败: %w", err)
	}
	nn, err := base64.StdEncoding.DecodeString(nonce)
	if err != nil {
		return "", fmt.Errorf("nonce base64解码失败: %w", err)
	}
	h := sha256.New()
	h.Write(ss)
	h.Write(nn)
	return base64.StdEncoding.EncodeToString(h.Sum(nil)), nil
}

// genSignature 生成请求签名
// signature = base64(hmac-sha256(key=base64decode(signedNonce), msg=uri&signedNonce&nonce&data=data))
func genSignature(uri, signedNonce, nonce, data string) (string, error) {
	key, err := base64.StdEncoding.DecodeString(signedNonce)
	if err != nil {
		return "", err
	}
	msg := uri + "&" + signedNonce + "&" + nonce + "&data=" + data
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte(msg))
	return base64.StdEncoding.EncodeToString(mac.Sum(nil)), nil
}

// randomNonce 生成16位随机 nonce（大小写字母+数字）
func randomNonce() string {
	const chars = "1234567890abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ"
	b := make([]byte, 16)
	for i := range b {
		b[i] = chars[rand.Intn(len(chars))]
	}
	// MiWu 中 nonce 需要 base64 编码后参与签名，但传输的是原始随机字符串
	// 看 Kotlin: getNonce() 返回原始随机串，签名时 base64decode(nonce)
	// 这意味着 nonce 本身必须是合法的 base64。原始16字符字母数字串恰好是合法base64
	return string(b)
}

// RandomDeviceID 生成16位十六进制设备ID
func RandomDeviceID() string {
	b := make([]byte, 8)
	rand.Read(b)
	return strings.ToUpper(hex.EncodeToString(b))
}

// stripJSONP 去除米家接口返回的 &&&START&&& 前缀
func stripJSONP(s string) string {
	if idx := strings.Index(s, "{"); idx >= 0 {
		return s[idx:]
	}
	return s
}
