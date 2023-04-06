package main

import (
	"crypto/tls"
	"fmt"
	"github.com/go-redis/redis"
	"github.com/spf13/viper"
	"github.com/svchost9913/go-proxy-tools/global"
	"github.com/tidwall/gjson"
	"go.uber.org/zap"
	"io"
	"io/ioutil"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"
)

var (
	redisPool *redis.Client //全局使用
	redisOnce sync.Once
	viperConf *viper.Viper //全局使用
)

func InitRedis() {
	redisOnce.Do(func() {
		viperConf = viper.New()
		viperConf.SetConfigFile("conf.ini")
		err := viperConf.ReadInConfig()
		if err != nil {
			panic(interface{}(fmt.Errorf("failed to read config file: %v", err)))
		}

		addr := viperConf.GetString("redis.addr")
		password := viperConf.GetString("redis.password")
		db := viperConf.GetInt("redis.db")
		poolSize := viperConf.GetInt("redis.poolSize")

		redisPool = redis.NewClient(&redis.Options{
			Addr:     addr,
			Password: password,
			DB:       db,
			PoolSize: poolSize,
		})
	})
}

func SaveToSet(key string, value string) error {
	pipe := redisPool.Pipeline()
	pipe.SAdd(key, value)
	_, err := pipe.Exec()
	if err != nil {
		return fmt.Errorf("failed to save to set: %v", err)
	}
	return nil
}

func fetch(url string) ([]string, error) {
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.TLSClientConfig = &tls.Config{InsecureSkipVerify: true}
	client := &http.Client{Transport: tr, Timeout: 30 * time.Second}
	resp, err := client.Get(url)
	//zap.S().Infof("请求url: %s", url)
	if err != nil {
		return nil, err
	}
	defer func(Body io.ReadCloser) {
		err := Body.Close()
		if err != nil {

		}
	}(resp.Body)
	body, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	result := gjson.Parse(string(body))
	proxies := result.Get("#.proxy").Array()
	var proxyList []string
	for _, proxy := range proxies {
		proxyList = append(proxyList, proxy.String())
	}
	return proxyList, nil
}

func fetchAll(urls []string, concurrency int) ([][]string, error) {
	var wg sync.WaitGroup
	semaphore := make(chan struct{}, concurrency)
	responses := make([][]string, len(urls))
	errors := make([]error, len(urls))
	for i, url := range urls {
		wg.Add(1)
		semaphore <- struct{}{}
		go func(i int, url string) {
			defer wg.Done()
			defer func() { <-semaphore }()
			body, err := fetch(url)
			if err != nil {
				errors[i] = err
			} else {
				responses[i] = body
				for _, proxy := range body {
					err = SaveToSet("proxy_set", proxy)
					if err != nil {
						zap.S().Errorf("Failed to save proxy to Redis: %v", err)
					}
				}
			}
		}(i, url)
	}

	wg.Wait()
	var err error
	for _, e := range errors {
		if e != nil {
			err = fmt.Errorf("%v\n%v", err, e)
		}
	}
	return responses, err
}

func readAllProxies() error {
	proxies, err := redisPool.SMembers("proxy_set").Result()
	if err != nil {
		return fmt.Errorf("failed to read proxies from Redis: %v", err)
	}
	f, err := os.Create("ip.txt")
	if err != nil {
		return fmt.Errorf("failed to create file: %v", err)
	}
	defer func(f *os.File) {
		err := f.Close()
		if err != nil {

		}
	}(f)
	for _, proxy := range proxies {
		_, err := f.WriteString(proxy + "\n")
		if err != nil {
			return fmt.Errorf("failed to write proxy to file: %v", err)
		}
	}
	return nil
}

func GetProxyUrl(url string) ([]string, error) {
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.TLSClientConfig = &tls.Config{InsecureSkipVerify: true}
	client := &http.Client{Transport: tr, Timeout: 60 * time.Second}
	resp, err := client.Get(url)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	body, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}

	proxies := strings.Split(string(body), "\n")
	for i, proxy := range proxies {
		if !strings.HasPrefix(proxy, "http") {
			proxies[i] = "http://" + proxy
		}
	}
	return proxies, nil
}

func main() {
	global.InitLogger()
	InitRedis()
	url := "https://domain.svchostok.pro/f?data=body=%22get%20all%20proxy%20from%20proxy%20pool%22"
	timer := time.NewTimer(7 * time.Hour) // 创建一个 3 小时的计时器
	for {
		select {
		case <-timer.C:
			urls, err := GetProxyUrl(url)
			zap.S().Infof("获取到url: %d条", len(urls))
			if err != nil {
				fmt.Println("Error:", err)
				return
			}
			_, err = fetchAll(urls, 5)
			if err != nil {
				fmt.Println("Error:", err)
				return
			}
			err = readAllProxies()
			if err != nil {
				fmt.Println("Error:", err)
				return
			}
			zap.S().Infof("采集完成!!!")
		}
	}
}
