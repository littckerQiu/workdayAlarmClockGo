/*
 * http服务路由 文档https://gin-gonic.com/zh-cn/docs/
 * zyyme 202305023
 * v1.0
 */

package router

import (
	"embed"
	"encoding/json"
	"io/fs"
	"log"
	"net/http"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
	"workdayAlarmClock/app"
	"workdayAlarmClock/conf"
	"workdayAlarmClock/mihome"
	"workdayAlarmClock/oversleep"
	"workdayAlarmClock/player"
	"workdayAlarmClock/weather"

	"github.com/gin-gonic/gin"
	"github.com/zanjie1999/httpme"
)

// 下面这个注释配置了需要打包进二进制文件的静态文件
//
//go:embed static/*
var f embed.FS

var (
	js2home = "\n<script>setInterval(function(){window.history.go(-1)},3000);</script><style>@media(prefers-color-scheme:dark){body{color:#e0e0e0;background-color:#121212;}</style>"
	js2back = "<script>window.history.go(-1)</script>"

	// 扫码登录状态
	qrLoginMu    sync.Mutex
	qrLoginState = ""
	qrLoginMsg   = ""
)

func parseNeID(s, typ string) string {
	if regexp.MustCompile(`^\d+$`).MatchString(s) {
		return s
	}
	if m := regexp.MustCompile(`(https?://)?163cn\.tv/[^\s"'<>，。；、)）]+`).FindStringSubmatch(s); m != nil {
		url := m[0]
		if !strings.HasPrefix(url, "http://") && !strings.HasPrefix(url, "https://") {
			url = "https://" + url
		}
		req := httpme.Httpme()
		if resp, err := req.Get(url); err == nil {
			resp.R.Body.Close()
			s = resp.R.Request.URL.String()
		} else {
			log.Println("解析网易云短链失败", err)
		}
	}
	if m := regexp.MustCompile(typ + `\?[^#]*?id=(\d+)`).FindStringSubmatch(s); m != nil {
		return m[1]
	}
	return s
}

func Init(urlPrefix string) *gin.Engine {
	r := gin.Default()
	r.MaxMultipartMemory = 4 << 20
	// 允许跨域
	r.Use(func(c *gin.Context) {
		c.Header("Access-Control-Allow-Origin", "*")
	})
	// 静态访问
	staticFs, err := fs.Sub(f, "static")
	if err != nil {
		log.Print("read static files error")
	}
	// 因为gin打死不修bug，只能这样访问index.html
	r.StaticFileFS("/", "./", http.FS(staticFs))
	r.StaticFileFS("/favicon.ico", "./favicon.ico", http.FS(staticFs))
	r.StaticFS("/static", http.FS(staticFs))

	// url prefix
	root := r.Group(urlPrefix)

	r.StaticFileFS("/alarm.html", "./alarm.html", http.FS(staticFs))
	root.StaticFile("/cfg.json", "./workdayAlarmClock.json")
	root.StaticFile("/weather.mp3", "./weather.mp3")
	// 允许直接浏览缓存
	if conf.Cfg.SavePath != "" {
		root.StaticFS("/music", gin.Dir(conf.Cfg.SavePath, true))
	}
	root.StaticFS("/sdcard", gin.Dir("/sdcard", true))

	// 删除缓存目录
	root.GET("/delSave", func(c *gin.Context) {
		if conf.Cfg.SavePath != "" && conf.Cfg.SavePath != "/" && conf.Cfg.SavePath != "./" {
			os.RemoveAll(conf.Cfg.SavePath)
			os.MkdirAll(conf.Cfg.SavePath, os.ModePerm)
			c.Data(http.StatusOK, "text/html; charset=utf-8", []byte("<h1>已删除</h1>"+js2home))
		} else {
			c.Data(http.StatusOK, "text/html; charset=utf-8", []byte("<h1>你的操作很危险，驳回</h1><br>"+conf.Cfg.SavePath+js2home))
		}
	})

	root.GET("/hello", func(c *gin.Context) {
		c.JSON(200, gin.H{
			"message": "Hello World!",
		})
	})

	root.GET("/prev", func(c *gin.Context) {
		c.Data(http.StatusOK, "text/html; charset=utf-8", []byte("<h2>"+player.Prev()+"</h2>"+js2home))
	})

	root.GET("/next", func(c *gin.Context) {
		// c.JSON(200, gin.H{
		// 	"message": player.Next(),
		// })
		c.Data(http.StatusOK, "text/html; charset=utf-8", []byte("<h2>"+player.Next()+"</h2>"+js2home))
	})

	root.GET("/stop", func(c *gin.Context) {
		player.Stop()
		// c.JSON(200, gin.H{
		// 	"message": "stop",
		// })
		c.Data(http.StatusOK, "text/html; charset=utf-8", []byte("<h1>stop</h1>"+js2home))
	})

	root.GET("/play", func(c *gin.Context) {
		url := c.Query("url")
		if url == "" {
			c.Data(http.StatusOK, "text/html; charset=utf-8", []byte("<h1>url is empty</h1>"+js2home))
			// 没有参数时一键播放
			if player.IsStop {
				player.Me1Key()
			} else if player.IsPaused {
				player.Resume()
			} else {
				player.Pause()
			}
			return
		}
		loopMode := c.Query("loopMode") != ""
		player.LoopMode = loopMode
		player.PlayUrl(url)
		s := "<h2>播放" + url + "</h2>"
		if loopMode {
			s += "<h1>单曲循环</h1>"
		}
		c.Data(http.StatusOK, "text/html; charset=utf-8", []byte(s+js2home))
	})

	// 一键急停按钮 自动控制播放停止
	root.GET("/1key", func(c *gin.Context) {
		c.Data(http.StatusOK, "text/html; charset=utf-8", []byte(player.Me1Key()))
	})

	root.GET("/playlist", func(c *gin.Context) {
		id := parseNeID(c.Query("id"), "playlist")
		if id == "" {
			c.Data(http.StatusOK, "text/html; charset=utf-8", []byte("<h1>id is empty</h1>"+js2home))
			return
		}
		c.Data(http.StatusOK, "text/html; charset=utf-8", []byte("<h1>播放歌单 "+player.PlayPlaylist(id, c.Query("random") == "1")+"</h1>"+js2home))
	})

	root.GET("/playmusic", func(c *gin.Context) {
		id := parseNeID(c.Query("id"), "song")
		if id == "" {
			c.Data(http.StatusOK, "text/html; charset=utf-8", []byte("<h1>id is empty</h1>"+js2home))
			return
		}
		loopMode := c.Query("loopMode") != ""
		player.PlayPlaymusic(id, loopMode)
		s := "<h1>播放歌曲" + id + "</h1>"
		if loopMode {
			s += "<h1>单曲循环</h1>"
		}
		c.Data(http.StatusOK, "text/html; charset=utf-8", []byte(s+js2home))
	})

	root.GET("/vol", func(c *gin.Context) {
		vol := c.Query("vol")
		if vol == "" {
			c.Data(http.StatusOK, "text/html; charset=utf-8", []byte("<h1>vol=1~100</h1>"+js2home))
			return
		}
		player.SetVol(vol)
		c.Data(http.StatusOK, "text/html; charset=utf-8", []byte("ok"+js2home))
	})

	root.GET("/echo", func(c *gin.Context) {
		msg := c.Query("msg")
		if msg == "" {
			c.Data(http.StatusOK, "text/html; charset=utf-8", []byte("<h1>msg is empty</h1>"+js2home))
			return
		}
		app.Send(msg)
		c.Data(http.StatusOK, "text/html; charset=utf-8", []byte("ok"+js2home))
	})

	// 暂停播放
	root.GET("/pause", func(c *gin.Context) {
		player.Pause()
		c.Data(http.StatusOK, "text/html; charset=utf-8", []byte(js2back))
	})

	// 恢复播放
	root.GET("/resume", func(c *gin.Context) {
		player.Resume()
		c.Data(http.StatusOK, "text/html; charset=utf-8", []byte(js2back))
	})

	// 音量加
	root.GET("/volp", func(c *gin.Context) {
		player.VolUp()
		c.Data(http.StatusOK, "text/html; charset=utf-8", []byte(js2back))
	})
	root.GET("/volpn", func(c *gin.Context) {
		// 暂停了这就变成下一首按钮了
		if player.IsPaused {
			player.Next()
		} else {
			player.VolUp()
		}
		c.Data(http.StatusOK, "text/html; charset=utf-8", []byte(js2back))
	})

	// 音量减
	root.GET("/volm", func(c *gin.Context) {
		player.VolDown()
		c.Data(http.StatusOK, "text/html; charset=utf-8", []byte(js2back))
	})
	root.GET("/volmp", func(c *gin.Context) {
		// 暂停了这就变成上一首按钮了
		if player.IsPaused {
			player.Prev()
		} else {
			player.VolDown()
		}
		c.Data(http.StatusOK, "text/html; charset=utf-8", []byte(js2back))
	})

	// 测试闹钟
	root.GET("/testAlarm", func(c *gin.Context) {
		player.PlayAlarm()
		c.Data(http.StatusOK, "text/html; charset=utf-8", []byte("<h1>闹钟时间到</h1>"+js2home))
	})

	// 加闹钟
	root.GET("/addAlarm", func(c *gin.Context) {
		hhmm := c.Query("hhmm")
		typeS := c.Query("type")
		// 万一呢 顺手就输进去了 手贱过一次导致闹钟没响
		hhmm = strings.ReplaceAll(strings.ReplaceAll(hhmm, "：", ""), ":", "")
		if len(typeS) > 0 && typeS[len(typeS)-1] == ',' {
			typeS = typeS[:len(typeS)-1]
		}
		if typeS == "" {
			// 默认一次
			typeS = "3"
		}
		if hhmm == "" {
			c.Data(http.StatusOK, "text/html; charset=utf-8", []byte("<h1>hhmm is empty</h1>"+js2home))
			return
		}
		if typeList, exists := conf.Cfg.Alarm[hhmm]; exists {
			conf.Cfg.Alarm[hhmm] = append(typeList, strings.Split(typeS, ",")...)
		} else {
			conf.Cfg.Alarm[hhmm] = strings.Split(typeS, ",")
		}
		conf.Save()
		updateAppAlarmWake()
		c.Data(http.StatusOK, "text/html; charset=utf-8", []byte(js2back))
	})

	// 删闹钟
	root.GET("/delAlarm", func(c *gin.Context) {
		hhmm := c.Query("hhmm")
		if hhmm == "" {
			c.Data(http.StatusOK, "text/html; charset=utf-8", []byte("<h1>hhmm is empty</h1>"+js2home))
			return
		}
		delete(conf.Cfg.Alarm, hhmm)
		conf.Save()
		updateAppAlarmWake()
		c.Data(http.StatusOK, "text/html; charset=utf-8", []byte(js2back))
	})

	// 跳过闹钟
	root.GET("/skipAlarm", func(c *gin.Context) {
		n := c.Query("n")
		if n == "0" {
			player.SkipAlarm = 0
		} else if n == "1" {
			player.SkipAlarm++
		}
		c.Data(http.StatusOK, "text/html; charset=utf-8", []byte("<h1>将跳过 "+strconv.Itoa(player.SkipAlarm)+" 次闹钟</h1>"+js2home))
	})

	// 更新设置
	root.GET("/updateCfg", func(c *gin.Context) {
		nePlayListId := c.Query("nePlayListId")
		defPlayListId := c.Query("defPlayListId")
		volAlarm := c.Query("volAlarm")
		VolDefault := c.Query("volDefault")
		Tz := c.Query("tz")
		WeatherCityCode := c.Query("weatherCityCode")
		WeatherUpdate := c.Query("weatherUpdate")
		wakelock := c.Query("wakelock")
		alarmTime := c.Query("alarmTime")
		muteWhenStop := c.Query("muteWhenStop")
		musicQuality := c.Query("musicQuality")
		savePath := c.Query("savePath")
		defSeek := c.Query("defSeek")
		broadcastMode := c.Query("broadcastMode")
		smallWeekDate := c.Query("smallWeekDate")
		// 睡过头检测相关
		oversleepEnable := c.Query("oversleepEnable")
		workIntervals := c.Query("workIntervals")
		workOnly := c.Query("workOnly")
		barkUrl := c.Query("barkUrl")
		barkInterval := c.Query("barkInterval")
		checkInterval := c.Query("oversleepCheckInterval")
		volRampStart := c.Query("volRampStart")
		volRampStep := c.Query("volRampStep")
		volRampInterval := c.Query("volRampInterval")
		volRampMax := c.Query("volRampMax")
		miSiid := c.Query("miSiid")
		miPiid := c.Query("miPiid")
		if nePlayListId != "" {
			conf.Cfg.NePlayListId = nePlayListId
		}
		if defPlayListId != "" {
			conf.Cfg.DefPlayListId = defPlayListId
		}
		if volAlarm != "" {
			conf.Cfg.VolAlarm = volAlarm
		}
		if VolDefault != "" {
			conf.Cfg.VolDefault = VolDefault
		}
		if Tz != "" {
			tz, err := strconv.Atoi(Tz)
			if err != nil {
				c.Data(http.StatusOK, "text/html; charset=utf-8", []byte("<h1>时区不是整数</h1>"+js2home))
				return
			} else {
				conf.Cfg.Tz = tz
				time.Local = time.FixedZone("UTC+", tz*3600)
			}
		}
		conf.Cfg.WeatherCityCode = WeatherCityCode
		conf.Cfg.WeatherUpdate = WeatherUpdate
		conf.Cfg.Wakelock = wakelock == "1"
		conf.Cfg.MuteWhenStop = muteWhenStop == "1"
		conf.Cfg.BroadcastMode = broadcastMode == "1"
		conf.Cfg.DefSeek = defSeek
		if conf.IsApp {
			app.SendLocal("DSEEK " + defSeek)
		}
		if alarmTime != "" {
			conf.Cfg.AlarmTime, _ = strconv.ParseFloat(alarmTime, 64)
		}
		conf.Cfg.MusicQuality = musicQuality
		if savePath == "" {
			conf.Cfg.SavePath = ""
		} else if savePath != "/" && savePath != "./" {
			conf.Cfg.SavePath = savePath
			if conf.IsApp && conf.Cfg.SavePath[0] != '/' && conf.Cfg.SavePath[0] != '.' && (len(conf.Cfg.SavePath) == 1 || conf.Cfg.SavePath[1] != ':') {
				conf.Cfg.SavePath = "./" + conf.Cfg.SavePath
			}
			if !strings.HasSuffix(conf.Cfg.SavePath, "/") {
				conf.Cfg.SavePath += "/"
			}
		} else {
			conf.Cfg.SavePath = "./music/"
		}
		if conf.Cfg.SavePath != "" {
			err := os.MkdirAll(conf.Cfg.SavePath, os.ModePerm)
			if err != nil {
				log.Println("创建缓存目录出错", conf.Cfg.SavePath, err)
				c.Data(http.StatusOK, "text/html; charset=utf-8", []byte("<h1>创建缓存目录出错"+conf.Cfg.SavePath+"</h1>"))
			}
		}
		if conf.Cfg.SmallWeekDate != smallWeekDate {
			conf.Cfg.SmallWeekDate = smallWeekDate
			if smallWeekDate != "" {
				conf.WorkDayApi()
			}
		}
		// 睡过头检测配置
		wasEnabled := conf.Cfg.OversleepEnable
		conf.Cfg.OversleepEnable = oversleepEnable == "1"
		if workIntervals != "" {
			var intervals []conf.WorkInterval
			if err := json.Unmarshal([]byte(workIntervals), &intervals); err == nil {
				// 规范化时间格式
				for i := range intervals {
					intervals[i].Start = strings.ReplaceAll(strings.ReplaceAll(intervals[i].Start, "：", ""), ":", "")
					intervals[i].End = strings.ReplaceAll(strings.ReplaceAll(intervals[i].End, "：", ""), ":", "")
				}
				conf.Cfg.WorkIntervals = intervals
			}
		}
		conf.Cfg.WorkOnly = workOnly == "1"
		if barkUrl != "" {
			conf.Cfg.BarkUrl = barkUrl
		}
		if barkInterval != "" {
			if v, err := strconv.Atoi(barkInterval); err == nil && v > 0 {
				conf.Cfg.BarkInterval = v
			}
		}
		if checkInterval != "" {
			if v, err := strconv.Atoi(checkInterval); err == nil && v > 0 {
				conf.Cfg.OversleepCheckInterval = v
			}
		}
		if volRampStart != "" {
			if v, err := strconv.Atoi(volRampStart); err == nil && v >= 0 && v <= 100 {
				conf.Cfg.VolRampStart = v
			}
		}
		if volRampStep != "" {
			if v, err := strconv.Atoi(volRampStep); err == nil && v > 0 {
				conf.Cfg.VolRampStep = v
			}
		}
		if volRampInterval != "" {
			if v, err := strconv.Atoi(volRampInterval); err == nil && v > 0 {
				conf.Cfg.VolRampInterval = v
			}
		}
		if volRampMax != "" {
			if v, err := strconv.Atoi(volRampMax); err == nil && v > 0 && v <= 100 {
				conf.Cfg.VolRampMax = v
			}
		}
		if miSiid != "" {
			if v, err := strconv.Atoi(miSiid); err == nil && v > 0 {
				conf.Cfg.MiSiid = v
			}
		}
		if miPiid != "" {
			if v, err := strconv.Atoi(miPiid); err == nil && v > 0 {
				conf.Cfg.MiPiid = v
			}
		}
		conf.Save()
		// 如果启用状态发生变化，重启监视器
		if wasEnabled != conf.Cfg.OversleepEnable {
			if conf.Cfg.OversleepEnable {
				oversleep.Start()
			} else {
				oversleep.Stop()
			}
		}
		c.Data(http.StatusOK, "text/html; charset=utf-8", []byte(js2back))
	})

	// 上传配置
	root.POST("/uploadCfg", func(c *gin.Context) {
		// 接收上传的file并保存
		file, _ := c.FormFile("file")
		if file == nil {
			c.Data(http.StatusOK, "text/html; charset=utf-8", []byte("<h1>file is empty</h1>"+js2home))
			return
		}
		c.SaveUploadedFile(file, "workdayAlarmClock.json")
		// 重新加载配置
		conf.Init()
		conf.WorkDayApi()
		updateAppAlarmWake()
		c.Data(http.StatusOK, "text/html; charset=utf-8", []byte("<h1>上传成功</h1>"+js2home))
	})

	// 上传兜底的mp3
	root.POST("/uploadMp3", func(c *gin.Context) {
		// 接收上传的file并保存
		file, _ := c.FormFile("file")
		if file == nil {
			c.Data(http.StatusOK, "text/html; charset=utf-8", []byte("<h1>file is empty</h1>"+js2home))
			return
		}
		c.SaveUploadedFile(file, "music.mp3")
		c.Data(http.StatusOK, "text/html; charset=utf-8", []byte("<h1>上传成功</h1>"+js2home))
	})

	// 删除上传的音乐使用默认兜底
	root.GET("/deleteMp3", func(c *gin.Context) {
		os.Remove("music.mp3")
		c.Data(http.StatusOK, "text/html; charset=utf-8", []byte("<h1>已删除</h1>"+js2home))
	})

	// 播放兜底音乐
	root.GET("/music.mp3", func(c *gin.Context) {
		if _, err := os.Stat("music.mp3"); os.IsNotExist(err) {
			c.FileFromFS("music.mp3", http.FS(staticFs))
		} else {
			c.File("music.mp3")
		}
	})

	// 当前状态
	root.GET("/status", func(c *gin.Context) {
		batLevel, _ := os.ReadFile("/sys/class/power_supply/battery/capacity")
		c.JSON(200, gin.H{
			"isStop":      player.IsStop,
			"isPaused":    player.IsPaused,
			"playList":    player.PlayList,
			"isAlarm":     player.IsAlarm,
			"isOversleep": player.IsOversleep,
			"nowUrl":      player.NowUrl,
			"prevUrl":     player.PrevUrl,
			"batLevel":    string(batLevel),
			"nowId":       player.NowId,
			"startUnix":   player.StartUnix,
			"stopUnix":    player.StopUnix,
			"skipAlarm":   player.SkipAlarm,
			"oversleep":   oversleep.Status(),
			"alarms":      conf.Cfg.Alarm,
		})
	})

	// 下次闹钟
	root.GET("/nextAlarm", func(c *gin.Context) {
		now := time.Now()
		type nextInfo struct {
			Time    string `json:"time"`
			Seconds int    `json:"seconds"`
			HasAlarm bool  `json:"hasAlarm"`
		}
		result := nextInfo{HasAlarm: false}
		var bestTime time.Time
		found := false
		for hhmm, dayTypes := range conf.Cfg.Alarm {
			if len(hhmm) != 4 {
				continue
			}
			h, err1 := strconv.Atoi(hhmm[:2])
			m, err2 := strconv.Atoi(hhmm[2:])
			if err1 != nil || err2 != nil || h > 23 || m > 59 {
				continue
			}
			for dayOffset := 0; dayOffset < 8; dayOffset++ {
				checkDay := now.AddDate(0, 0, dayOffset)
				checkTime := time.Date(checkDay.Year(), checkDay.Month(), checkDay.Day(), h, m, 0, 0, now.Location())
				if checkTime.Before(now) || checkTime.Equal(now) {
					continue
				}
				weekday := int(checkDay.Weekday()) // 0=Sun
				isWorkDay := weekday != 0 && weekday != 6
				matches := false
				for _, dt := range dayTypes {
					switch dt {
					case "1":
						if isWorkDay {
							matches = true
						}
					case "2":
						if !isWorkDay {
							matches = true
						}
					case "3", "4":
						matches = true
					case "5", "6", "7", "8", "9", "10", "11":
						dtn, _ := strconv.Atoi(dt)
						if dtn-5 == weekday {
							matches = true
						}
					}
					if matches {
						break
					}
				}
				if matches && (!found || checkTime.Before(bestTime)) {
					bestTime = checkTime
					found = true
				}
				break
			}
		}
		if found {
			result.HasAlarm = true
			result.Time = bestTime.Format("15:04")
			result.Seconds = int(bestTime.Sub(now).Seconds())
		}
		c.JSON(200, result)
	})

	// 天气api
	root.GET("/getWeatherCityCode", func(c *gin.Context) {
		q := c.Query("q")
		code, _, err := weather.GetCityCode(q)
		if err != nil {
			c.Data(http.StatusOK, "text/html; charset=utf-8", []byte(err.Error()))
		} else {
			c.Data(http.StatusOK, "text/html; charset=utf-8", []byte(code))
		}
	})

	root.GET("/getWeather", func(c *gin.Context) {
		code := c.Query("code")
		c.Data(http.StatusOK, "text/html; charset=utf-8", []byte(weather.GetWeather(code)))
	})

	root.GET("/downWeather", func(c *gin.Context) {
		player.DownWeather()
		c.Data(http.StatusOK, "text/html; charset=utf-8", []byte("<h1>下载完毕</h1>"+js2home))
	})

	root.GET("/restart", func(c *gin.Context) {
		// 做不到的，因为要运行完这个方法才会返回
		// c.Data(http.StatusOK, "text/html; charset=utf-8", []byte(js2back))
		app.Send("RESTART")
		os.Exit(0)
	})

	// 自动停止
	root.GET("/timeStop", func(c *gin.Context) {
		unix := c.Query("unix")
		min := c.Query("min")
		if unix != "" {
			player.StopUnix, err = strconv.ParseInt(unix, 10, 64)
			if err != nil {
				c.Data(http.StatusOK, "text/html; charset=utf-8", []byte("unix不是整数"))
				return
			}
		} else if min != "" {
			minF, err := strconv.ParseFloat(min, 64)
			if err != nil {
				c.Data(http.StatusOK, "text/html; charset=utf-8", []byte("min不是浮点"))
				return
			}
			player.StopUnix = time.Now().Unix() + int64(minF*60)
		} else {
			player.StopUnix = 0
		}
		if player.StopUnix == 0 {
			c.Data(http.StatusOK, "text/html; charset=utf-8", []byte("<h2>定时停止已取消</h2>"+js2home))
		} else {
			c.Data(http.StatusOK, "text/html; charset=utf-8", []byte("<h2>将在 "+time.Unix(player.StopUnix, 0).Format("2006-01-02 15:04:05")+" 后停止</h2>"+js2home))
		}
	})

	// ===== 米家 & 睡过头检测 API =====

	// 米家登录
	root.GET("/mi/login", func(c *gin.Context) {
		user := c.Query("user")
		pass := c.Query("pass")
		if user == "" || pass == "" {
			// 也允许用保存的账号密码登录
			user = conf.Cfg.MiUser
			pass = conf.Cfg.MiPass
		}
		if user == "" || pass == "" {
			c.JSON(200, gin.H{"ok": false, "msg": "请提供米家账号和密码"})
			return
		}
		u, err := mihome.Login(user, pass)
		if err != nil {
			c.JSON(200, gin.H{"ok": false, "msg": err.Error()})
			return
		}
		// 保存凭证
		conf.Cfg.MiUser = user
		conf.Cfg.MiPass = pass
		conf.Cfg.MiServiceToken = u.ServiceToken
		conf.Cfg.MiSSecurity = u.SSecurity
		conf.Cfg.MiUserID = u.UserID
		conf.Cfg.MiDeviceID = u.DeviceID
		conf.Save()
		c.JSON(200, gin.H{"ok": true, "msg": "登录成功", "userId": u.UserID})
	})

	// 米家登录状态
	root.GET("/mi/status", func(c *gin.Context) {
		c.JSON(200, gin.H{
			"loggedIn":   mihome.IsLoggedIn(),
			"userId":     conf.Cfg.MiUserID,
			"homeId":     conf.Cfg.MiHomeID,
			"deviceDid":  conf.Cfg.MiDeviceDid,
			"deviceName": conf.Cfg.MiDeviceName,
		})
	})

	// 扫码登录：生成二维码
	root.GET("/mi/qr/generate", func(c *gin.Context) {
		qr, err := mihome.GenerateQrCode()
		if err != nil {
			c.JSON(200, gin.H{"ok": false, "msg": err.Error()})
			return
		}
		// 启动后台 goroutine 进行长轮询
		go func(pollUrl string) {
			qrLoginMu.Lock()
			qrLoginState = "waiting"
			qrLoginMsg = ""
			qrLoginMu.Unlock()
			u, err := mihome.PollQrLogin(pollUrl)
			qrLoginMu.Lock()
			defer qrLoginMu.Unlock()
			if err != nil {
				if strings.HasPrefix(err.Error(), "QR_LOGIN_WAITING:") {
					qrLoginState = "timeout"
					qrLoginMsg = "二维码已过期，请刷新"
				} else {
					qrLoginState = "error"
					qrLoginMsg = err.Error()
				}
				return
			}
			conf.Cfg.MiServiceToken = u.ServiceToken
			conf.Cfg.MiSSecurity = u.SSecurity
			conf.Cfg.MiUserID = u.UserID
			conf.Cfg.MiDeviceID = u.DeviceID
			conf.Save()
			qrLoginState = "success"
			qrLoginMsg = "登录成功"
		}(qr.PollUrl)
		c.JSON(200, gin.H{"ok": true, "qrData": qr.QrData})
	})

	// 扫码登录：查询状态
	root.GET("/mi/qr/status", func(c *gin.Context) {
		qrLoginMu.Lock()
		state := qrLoginState
		msg := qrLoginMsg
		qrLoginMu.Unlock()
		c.JSON(200, gin.H{
			"ok":       state == "success",
			"state":    state,
			"msg":      msg,
			"loggedIn": mihome.IsLoggedIn(),
		})
	})

	// 获取家庭列表
	root.GET("/mi/homes", func(c *gin.Context) {
		if !mihome.IsLoggedIn() {
			c.JSON(200, gin.H{"ok": false, "msg": "未登录"})
			return
		}
		homes, err := mihome.GetHomes()
		if err != nil {
			c.JSON(200, gin.H{"ok": false, "msg": err.Error()})
			return
		}
		c.JSON(200, gin.H{"ok": true, "homes": homes})
	})

	// 获取设备列表
	root.GET("/mi/devices", func(c *gin.Context) {
		if !mihome.IsLoggedIn() {
			c.JSON(200, gin.H{"ok": false, "msg": "未登录"})
			return
		}
		homeID := c.Query("homeId")
		ownerStr := c.Query("owner")
		if homeID == "" {
			homeID = conf.Cfg.MiHomeID
			ownerStr = strconv.FormatInt(conf.Cfg.MiHomeOwner, 10)
		}
		owner, err := strconv.ParseInt(ownerStr, 10, 64)
		if err != nil {
			c.JSON(200, gin.H{"ok": false, "msg": "owner参数错误"})
			return
		}
		devices, err := mihome.GetDevices(homeID, owner)
		if err != nil {
			c.JSON(200, gin.H{"ok": false, "msg": err.Error()})
			return
		}
		c.JSON(200, gin.H{"ok": true, "devices": devices})
	})

	// 选择家庭和设备
	root.GET("/mi/select", func(c *gin.Context) {
		homeID := c.Query("homeId")
		ownerStr := c.Query("owner")
		did := c.Query("did")
		name := c.Query("name")
		if homeID != "" {
			conf.Cfg.MiHomeID = homeID
			owner, err := strconv.ParseInt(ownerStr, 10, 64)
			if err == nil {
				conf.Cfg.MiHomeOwner = owner
			}
		}
		if did != "" {
			conf.Cfg.MiDeviceDid = did
			conf.Cfg.MiDeviceName = name
		}
		conf.Save()
		c.JSON(200, gin.H{"ok": true})
	})

	// 查询设备开关状态
	root.GET("/mi/devicestatus", func(c *gin.Context) {
		if !mihome.IsLoggedIn() {
			c.JSON(200, gin.H{"ok": false, "msg": "未登录"})
			return
		}
		did := c.Query("did")
		if did == "" {
			did = conf.Cfg.MiDeviceDid
		}
		if did == "" {
			c.JSON(200, gin.H{"ok": false, "msg": "未选择设备"})
			return
		}
		siid := conf.Cfg.MiSiid
		piid := conf.Cfg.MiPiid
		if siid <= 0 {
			siid = 2
		}
		if piid <= 0 {
			piid = 1
		}
		isOn, err := mihome.IsDeviceOn(did, siid, piid)
		if err != nil {
			c.JSON(200, gin.H{"ok": false, "msg": err.Error()})
			return
		}
		c.JSON(200, gin.H{"ok": true, "isOn": isOn})
	})

	// 睡过头检测状态
	root.GET("/oversleep/status", func(c *gin.Context) {
		c.JSON(200, oversleep.Status())
	})

	// 手动触发一次检查
	root.GET("/oversleep/check", func(c *gin.Context) {
		go oversleep.CheckOnce()
		c.JSON(200, gin.H{"ok": true})
	})

	// 测试睡过头响铃
	root.GET("/oversleep/test", func(c *gin.Context) {
		player.PlayOversleepAlarm()
		c.Data(http.StatusOK, "text/html; charset=utf-8", []byte("<h1>睡过头闹钟测试中</h1>"+js2home))
	})

	// 停止睡过头响铃
	root.GET("/oversleep/stop", func(c *gin.Context) {
		player.Stop()
		c.Data(http.StatusOK, "text/html; charset=utf-8", []byte("<h1>已停止</h1>"+js2home))
	})

	// 测试 Bark 推送
	root.GET("/oversleep/testBark", func(c *gin.Context) {
		go oversleep.TestBark()
		c.JSON(200, gin.H{"ok": true, "msg": "Bark测试已发送"})
	})

	return r
}

// 更新app中每分钟锁的开关状态
func updateAppAlarmWake() {
	if conf.IsApp {
		if len(conf.Cfg.Alarm) > 0 {
			app.Send("ALARMON")
		} else {
			app.Send("ALARMOFF")
		}
	}
}
