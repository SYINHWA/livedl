
package niconico

import (
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"../cryptoconf"
	"../options"
	"io/ioutil"
	"regexp"
	"strconv"
	"os"
	"net"
	"encoding/xml"
	"bufio"
	"os/signal"
	"syscall"
)

func NicoLogin(id, pass string, opt options.Option) (err error) {
	if id == "" || pass == "" {
		err = fmt.Errorf("Login ID/Password not set. Use -nico-login \"<id>,<password>\"")
		return
	}
	tr := &http.Transport {
	//	IdleConnTimeout: 10 * time.Second,
	}
	client := &http.Client{Transport: tr}

	values := url.Values{"mail_tel": {id}, "password": {pass}, "site": {"nicoaccountsdk"}}
	req, _ := http.NewRequest("POST", "https://account.nicovideo.jp/api/v1/login", strings.NewReader(values.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded; charset=utf-8")

	resp, err := client.Do(req)
	if err != nil {
		return
	}
	defer resp.Body.Close()

	body, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		return
	}

	if ma := regexp.MustCompile(`<session_key>(.+?)</session_key>`).FindSubmatch(body); len(ma) > 0 {
		data := map[string]string{"NicoSession": string(ma[1])}
		if err = cryptoconf.Set(data, opt.ConfFile, opt.ConfPass); err != nil {
			return
		}
		fmt.Println("login success")
	} else {
		err = fmt.Errorf("login failed: session_key not found")
		return
	}
	return
}

func Record(opt options.Option) (err error) {

	for i := 0; i < 2; i++ {
		// load session info
		if data, e := cryptoconf.Load(opt.ConfFile, opt.ConfPass); e != nil {
			err = e
			return
		} else {
			opt.NicoLoginId, _ = data["NicoLoginId"].(string)
			opt.NicoLoginPass, _ = data["NicoLoginPass"].(string)
			opt.NicoSession, _ = data["NicoSession"].(string)
		}

		if (! opt.NicoRtmpOnly) {
			done, notLogin, e := NicoRecHls(opt)
			if done {
				return
			}
			if e != nil {
				err = e
				return
			}
			if notLogin {
				fmt.Println("not_login")
				if err = NicoLogin(opt.NicoLoginId, opt.NicoLoginPass, opt); err != nil {
					return
				}
				continue
			}
		}

		if (! opt.NicoHlsOnly) {
			notLogin, e := NicoRecRtmp(opt)
			if e != nil {
				err = e
				return
			}
			if notLogin {
				fmt.Println("not_login")
				if err = NicoLogin(opt.NicoLoginId, opt.NicoLoginPass, opt); err != nil {
					return
				}
				continue
			}
		}

		break
	}

	return
}

func TestRun(opt options.Option) (err error) {
	if false {
		ch := make(chan os.Signal, 10)
		signal.Notify(ch, os.Interrupt, syscall.SIGTERM, syscall.SIGINT)
		go func() {
			<-ch
			os.Exit(0)
		}()
	}

	opt.NicoRtmpIndex = map[int]bool{
		0: true,
	}

	var nextId func() string

	if opt.NicoLiveId == "" {
		// niconama alert

		if opt.NicoTestTimeout <= 0 {
			opt.NicoTestTimeout = 12
		}

		resp, e := http.Get("http://live.nicovideo.jp/api/getalertinfo")
		if e != nil {
			err = e
			return
		}
		defer resp.Body.Close()

		switch resp.StatusCode {
		case 200:
		default:
			err = fmt.Errorf("StatusCode is %v", resp.StatusCode)
			return
		}

		type Alert struct {
			User     string `xml:"user_id"`
			UserHash string `xml:"user_hash"`
			Addr     string `xml:"ms>addr"`
			Port     string `xml:"ms>port"`
			Thread   string `xml:"ms>thread"`
		}
		status := &Alert{}
		dat, _ := ioutil.ReadAll(resp.Body)
		resp.Body.Close()

		err = xml.Unmarshal(dat, status)
		if err != nil {
			fmt.Println(string(dat))
			fmt.Printf("error: %v", err)
			return
		}

		raddr, e := net.ResolveTCPAddr("tcp", fmt.Sprintf("%s:%s", status.Addr, status.Port))
		if e != nil {
			fmt.Printf("%v\n", e)
			return
		}

		conn, e := net.DialTCP("tcp", nil, raddr)
		if e != nil {
			err = e
			return
		}
		defer conn.Close()

		msg := fmt.Sprintf(`<thread thread="%s" version="20061206" res_from="-1"/>%c`, status.Thread, 0)
		if _, err = conn.Write([]byte(msg)); err != nil {
			fmt.Println(err)
			return
		}

		rdr := bufio.NewReader(conn)

		chLatest := make(chan string, 1000)
		go func(){
			for {
				s, e := rdr.ReadString(0)
				if e != nil {
					fmt.Println(e)
					err = e
					return
				}
				//fmt.Println(s)
				if ma := regexp.MustCompile(`>(\d+),\S+,\S+<`).FindStringSubmatch(s); len(ma) > 0 {
					L0:for {
						select {
							case <-chLatest:
							default:
								break L0
						}
					}
					chLatest <- ma[1]
				}
			}
		}()

		nextId = func() (string) {
			L1:for {
				select {
					case <-chLatest:
					default:
						break L1
				}
			}
			return <-chLatest
		}

	} else {
		// start from NicoLiveId
		var id int64
		if ma := regexp.MustCompile(`\Alv(\d+)\z`).FindStringSubmatch(opt.NicoLiveId); len(ma) > 0 {
			if id, err = strconv.ParseInt(ma[1], 10, 64); err != nil {
				fmt.Println(err)
				return
			}
		} else {
			fmt.Println("TestRun: NicoLiveId not specified")
			return
		}

		nextId = func() (s string) {
			s = fmt.Sprintf("%d", id)
			id++
			return
		}
	}

	if opt.NicoTestTimeout <= 0 {
		opt.NicoTestTimeout = 3
	}

	//chErr := make(chan error)
	var NFCount int
	var endCount int
	for {
		opt.NicoLiveId = fmt.Sprintf("lv%s", nextId())

		fmt.Fprintf(os.Stderr, "start test: %s\n", opt.NicoLiveId)
		var msg string
		err = Record(opt)
		if err != nil {
			if ma := regexp.MustCompile(`\AError\s+code:\s*(\S+)`).FindStringSubmatch(err.Error()); len(ma) > 0 {
				msg = ma[1]
				switch ma[1] {
				case "notfound", "closed", "comingsoon", "timeshift_ticket_exhaust":
				case "deletedbyuser", "deletedbyvisor", "violated":
				case "usertimeshift", "tsarchive", "require_community_member",
				     "noauth", "full", "premium_only", "selected-country":
				default:
					fmt.Fprintf(os.Stderr, "unknown: %s\n", ma[1])
					return
				}

			} else if strings.Contains(err.Error(), "closed network") {
				msg = "OK"
			} else {
				fmt.Fprintln(os.Stderr, err)
				return
			}
		} else {
			msg = "OK"
		}

		fmt.Fprintf(os.Stderr, "%s: %s\n---------\n", opt.NicoLiveId, msg)
		endCount++
		if endCount > 100 {
			break
		}

		if msg == "notfound" {
			NFCount++
		} else {
			NFCount = 0
		}
		if NFCount >= 10 {
			return
		}
	}
	return
}
