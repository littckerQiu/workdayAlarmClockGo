/*
 * 睡过头检测
 * 在工作时间区间内检测米家设备（如空调）是否仍处于开启状态，
 * 若开启则判定为睡过头/忘关设备，持续响铃并通过 Bark 推送手机通知，
 * 音量随时间逐渐增大，设备关闭后立刻停止响铃。
 * zyyme 20260828
 */
package oversleep

import (
	"fmt"
	"log"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
	"workdayAlarmClock/conf"
	"workdayAlarmClock/mihome"
	"workdayAlarmClock/player"
)

var (
	mu              sync.Mutex
	checkMu         sync.Mutex
	running         = false
	lastBarkUnix    int64
	lastVolIncrease int64
	currentVol      int
	stopChan        chan struct{}
)

// Start 启动睡过头监视器（幂等）
func Start() {
	mu.Lock()
	defer mu.Unlock()
	if running {
		return
	}
	running = true
	stopChan = make(chan struct{})
	go monitor()
	log.Println("睡过头检测监视器已启动")
}

// Stop 停止监视器
func Stop() {
	mu.Lock()
	defer mu.Unlock()
	if !running {
		return
	}
	close(stopChan)
	running = false
	if player.IsOversleep {
		player.Stop()
	}
	log.Println("睡过头检测监视器已停止")
}

// Restart 重启监视器（配置变更后调用）
func Restart() {
	Stop()
	Start()
}

func monitor() {
	// 立即检查一次，然后按间隔定时检查
	check()
	interval := getCheckInterval()
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-stopChan:
			return
		case <-ticker.C:
			// 动态调整检查间隔
			newInterval := getCheckInterval()
			if newInterval != interval {
				interval = newInterval
				ticker.Reset(interval)
			}
			check()
		}
	}
}

func getCheckInterval() time.Duration {
	sec := conf.Cfg.OversleepCheckInterval
	if sec <= 0 {
		sec = 30
	}
	return time.Duration(sec) * time.Second
}

// check 执行一次设备状态检查
func check() {
	checkMu.Lock()
	defer checkMu.Unlock()

	// 未启用或未配置设备
	if !conf.Cfg.OversleepEnable {
		// 如果之前在响但配置被关闭，停止
		if player.IsOversleep {
			log.Println("睡过头检测已关闭，停止响铃")
			player.Stop()
		}
		return
	}
	if conf.Cfg.MiDeviceDid == "" {
		return
	}

	// 确保米家已登录
	if !mihome.IsLoggedIn() {
		if err := autoLogin(); err != nil {
			log.Println("米家自动登录失败:", err)
			return
		}
	}

	// 检查是否在工作时间区间内
	inInterval, err := inWorkInterval(time.Now())
	if err != nil {
		log.Println("工作区间配置错误:", err)
		return
	}

	if !inInterval {
		// 不在工作区间，如果正在响铃则停止
		if player.IsOversleep {
			log.Println("已超出工作时间区间，停止睡过头响铃")
			player.Stop()
			resetState()
		}
		return
	}

	// 仅工作日模式
	if conf.Cfg.WorkOnly && !conf.IsWorkDay {
		if player.IsOversleep {
			log.Println("今天非工作日，停止睡过头响铃")
			player.Stop()
			resetState()
		}
		return
	}

	// 查询设备开关状态
	siid := conf.Cfg.MiSiid
	piid := conf.Cfg.MiPiid
	if siid <= 0 {
		siid = 2
	}
	if piid <= 0 {
		piid = 1
	}
	isOn, err := mihome.IsDeviceOn(conf.Cfg.MiDeviceDid, siid, piid)
	if err != nil {
		log.Println("查询设备状态失败:", err)
		// 如果正在响铃但查询失败，不停止（可能是临时网络问题）
		// 如果没在响铃，不触发（避免误报）
		return
	}

	if isOn {
		// 设备开着 → 睡过头/忘关
		if !player.IsOversleep {
			log.Println("工作时间内设备仍开启，启动睡过头响铃")
			startOversleep()
		} else {
			// 已经在响，检查音量渐强
			rampVolume()
			// 定期重发 Bark
			sendBarkIfDue()
		}
	} else {
		// 设备已关 → 立刻停止响铃
		if player.IsOversleep {
			log.Println("设备已关闭，立刻停止响铃")
			player.Stop()
			resetState()
		}
	}
}

// startOversleep 启动睡过头响铃和通知
func startOversleep() {
	currentVol = conf.Cfg.VolRampStart
	if currentVol <= 0 {
		currentVol, _ = strconv.Atoi(conf.Cfg.VolAlarm)
	}
	if currentVol <= 0 {
		currentVol = 50
	}
	lastVolIncrease = time.Now().Unix()
	lastBarkUnix = 0 // 立即发送一次 Bark
	player.PlayOversleepAlarm()
	sendBarkIfDue()
}

// rampVolume 音量渐强
func rampVolume() {
	step := conf.Cfg.VolRampStep
	interval := conf.Cfg.VolRampInterval
	maxVol := conf.Cfg.VolRampMax
	if step <= 0 || interval <= 0 {
		return
	}
	if maxVol <= 0 || maxVol > 100 {
		maxVol = 100
	}
	now := time.Now().Unix()
	if now-lastVolIncrease >= int64(interval) {
		currentVol += step
		if currentVol > maxVol {
			currentVol = maxVol
		}
		log.Println("睡过头闹钟音量渐强至", currentVol)
		player.SetOversleepVol(currentVol)
		lastVolIncrease = now
	}
}

// sendBarkIfDue 按间隔发送 Bark 通知
func sendBarkIfDue() {
	if conf.Cfg.BarkUrl == "" {
		return
	}
	interval := conf.Cfg.BarkInterval
	if interval <= 0 {
		interval = 300
	}
	now := time.Now().Unix()
	if now-lastBarkUnix < int64(interval) {
		return
	}
	lastBarkUnix = now
	go sendBark()
}

// sendBark 发送 Bark 推送
func sendBark() {
	deviceName := conf.Cfg.MiDeviceName
	if deviceName == "" {
		deviceName = "设备"
	}
	title := "⚠️ 睡过头警告"
	body := fmt.Sprintf("工作时间内%s仍未关闭，请立即关闭！", deviceName)

	barkUrl := strings.TrimRight(conf.Cfg.BarkUrl, "/")
	// 用户的 Bark 链接格式: https://api.day.app/KEY/内容
	// 支持直接填 key 或完整 URL
	var reqUrl string
	if strings.HasPrefix(barkUrl, "http") {
		// 拆分 URL，如果已有路径部分（如 /内容），去掉后重建
		parts := strings.SplitN(barkUrl, "/", 4)
		if len(parts) >= 3 {
			reqUrl = parts[0] + "//" + parts[2] + "/" + url.PathEscape(title) + "/" + url.PathEscape(body)
		} else {
			reqUrl = barkUrl + "/" + url.PathEscape(title) + "/" + url.PathEscape(body)
		}
	} else {
		reqUrl = "https://api.day.app/" + barkUrl + "/" + url.PathEscape(title) + "/" + url.PathEscape(body)
	}

	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Get(reqUrl)
	if err != nil {
		log.Println("Bark推送失败:", err)
		return
	}
	defer resp.Body.Close()
	log.Println("Bark推送已发送")
}

// inWorkInterval 判断当前时间是否在任意一个工作区间内
// 支持跨天区间（如 22:00-06:00）和多个区间
func inWorkInterval(now time.Time) (bool, error) {
	intervals := conf.Cfg.WorkIntervals
	if len(intervals) == 0 {
		return false, fmt.Errorf("工作区间未配置")
	}
	currentMin := now.Hour()*60 + now.Minute()
	for _, iv := range intervals {
		if iv.Start == "" || iv.End == "" {
			continue
		}
		startMin, err := hhmmToMinutes(iv.Start)
		if err != nil {
			continue
		}
		endMin, err := hhmmToMinutes(iv.End)
		if err != nil {
			continue
		}
		if startMin == endMin {
			continue
		}
		var in bool
		if startMin < endMin {
			in = currentMin >= startMin && currentMin < endMin
		} else {
			in = currentMin >= startMin || currentMin < endMin
		}
		if in {
			return true, nil
		}
	}
	return false, nil
}

// hhmmToMinutes 将 "HHmm" 转换为分钟数
func hhmmToMinutes(hhmm string) (int, error) {
	hhmm = strings.TrimSpace(strings.ReplaceAll(strings.ReplaceAll(hhmm, ":", ""), "：", ""))
	if len(hhmm) != 4 {
		return 0, fmt.Errorf("时间格式应为HHmm，收到: %s", hhmm)
	}
	h, err := strconv.Atoi(hhmm[:2])
	if err != nil {
		return 0, err
	}
	m, err := strconv.Atoi(hhmm[2:])
	if err != nil {
		return 0, err
	}
	if h < 0 || h > 23 || m < 0 || m > 59 {
		return 0, fmt.Errorf("时间超出范围: %s", hhmm)
	}
	return h*60 + m, nil
}

// autoLogin 尝试用保存的凭证自动登录米家
func autoLogin() error {
	// 优先使用已保存的 token
	if conf.Cfg.MiServiceToken != "" && conf.Cfg.MiSSecurity != "" && conf.Cfg.MiUserID != "" {
		deviceID := conf.Cfg.MiDeviceID
		if deviceID == "" {
			deviceID = mihome.RandomDeviceID()
			conf.Cfg.MiDeviceID = deviceID
		}
		mihome.SetUser(&mihome.User{
			UserID:       conf.Cfg.MiUserID,
			SSecurity:    conf.Cfg.MiSSecurity,
			ServiceToken: conf.Cfg.MiServiceToken,
			DeviceID:     deviceID,
		})
		// 验证 token 是否有效
		if _, err := mihome.GetHomes(); err == nil {
			log.Println("使用保存的米家凭证登录成功")
			return nil
		} else {
			log.Println("保存的米家凭证已失效，尝试重新登录:", err)
		}
	}
	// 用账号密码重新登录
	if conf.Cfg.MiUser != "" && conf.Cfg.MiPass != "" {
		u, err := mihome.Login(conf.Cfg.MiUser, conf.Cfg.MiPass)
		if err != nil {
			return err
		}
		// 保存凭证
		conf.Cfg.MiServiceToken = u.ServiceToken
		conf.Cfg.MiSSecurity = u.SSecurity
		conf.Cfg.MiUserID = u.UserID
		conf.Cfg.MiDeviceID = u.DeviceID
		conf.Save()
		log.Println("米家账号登录成功，凭证已保存")
		return nil
	}
	return fmt.Errorf("米家未登录且无账号密码")
}

// resetState 重置响铃状态
func resetState() {
	lastBarkUnix = 0
	lastVolIncrease = 0
	currentVol = 0
}

// IsActive 返回睡过头响铃是否正在进行
func IsActive() bool {
	return player.IsOversleep
}

// CheckOnce 手动触发一次检查
func CheckOnce() {
	check()
}

// TestBark 发送一条测试 Bark 通知
func TestBark() {
	deviceName := conf.Cfg.MiDeviceName
	if deviceName == "" {
		deviceName = "设备"
	}
	title := "🔔 测试通知"
	body := fmt.Sprintf("这是一条来自工作咩闹钟的测试通知，%s监控功能正常。", deviceName)

	barkUrl := strings.TrimRight(conf.Cfg.BarkUrl, "/")
	var reqUrl string
	if strings.HasPrefix(barkUrl, "http") {
		parts := strings.SplitN(barkUrl, "/", 4)
		if len(parts) >= 3 {
			reqUrl = parts[0] + "//" + parts[2] + "/" + url.PathEscape(title) + "/" + url.PathEscape(body)
		} else {
			reqUrl = barkUrl + "/" + url.PathEscape(title) + "/" + url.PathEscape(body)
		}
	} else {
		reqUrl = "https://api.day.app/" + barkUrl + "/" + url.PathEscape(title) + "/" + url.PathEscape(body)
	}
	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Get(reqUrl)
	if err != nil {
		log.Println("Bark测试失败:", err)
		return
	}
	defer resp.Body.Close()
	log.Println("Bark测试已发送")
}

// Status 返回睡过头检测的当前状态信息（供 WebUI 显示）
func Status() map[string]interface{} {
	return map[string]interface{}{
		"enabled":              conf.Cfg.OversleepEnable,
		"active":               player.IsOversleep,
		"deviceDid":            conf.Cfg.MiDeviceDid,
		"deviceName":           conf.Cfg.MiDeviceName,
		"workIntervals":        conf.Cfg.WorkIntervals,
		"workOnly":             conf.Cfg.WorkOnly,
		"barkUrl":              conf.Cfg.BarkUrl,
		"barkInterval":         conf.Cfg.BarkInterval,
		"checkInterval":        conf.Cfg.OversleepCheckInterval,
		"volRampStart":         conf.Cfg.VolRampStart,
		"volRampStep":          conf.Cfg.VolRampStep,
		"volRampInterval":      conf.Cfg.VolRampInterval,
		"volRampMax":           conf.Cfg.VolRampMax,
		"miSiid":               conf.Cfg.MiSiid,
		"miPiid":               conf.Cfg.MiPiid,
		"currentVol":           currentVol,
		"miLoggedIn":           mihome.IsLoggedIn(),
	}
}
