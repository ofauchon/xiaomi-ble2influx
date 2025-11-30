package main

import (
	"context"
	"encoding/hex"
	"flag"
	"fmt"
	"log"
	"os"
	"os/user"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/go-ble/ble"
	"github.com/go-ble/ble/examples/lib/dev"
	"github.com/pkg/errors"

	"github.com/davecgh/go-spew/spew"

	influxdb2 "github.com/influxdata/influxdb-client-go/v2"
	influxdb2api "github.com/influxdata/influxdb-client-go/v2/api"

	"gopkg.in/ini.v1"
)

type MijiaDeviceConfig struct {
	Mac   string `json:"mac"`
	Model string `json:"model"`
	Name  string `json:"name"`
	Desc  string `json:"desc"`
}

type MijiaMetrics struct {
	Mac        [6]byte
	Temp       float32
	Humi       float32
	Batt       float32
	RSSI       int
	FrameCount uint8
}

type Configuration struct {
	influx_url         string
	influx_token       string
	influx_bucket      string
	influx_org         string
	influx_measurement string
	period             int
	debugLevel         int
	logFile            string
	device             string
}

var (
	configFile *string
	dryrun     *bool
	config     Configuration

	/*
		// Args
		influx_server       = flag.String("influx-server", "http://localhost:8086", "Sets the influxDB server")
		influx_token        = flag.String("influx-token", "", "Sets the influxDB token")
		influx_org          = flag.String("influx-org", "your.org", "Sets the influxDB organization")
		influx_bucket       = flag.String("influx-bucket", "default", "Sets the influxDB bucket")
		influx_measurement  = flag.String("influx-measurement", "metro", "Sets the influxDB measurement name")
		dropuser            = flag.String("user", "default", "Drop privileges to <user>")
		sensors_descriptor  = flag.String("desc", "~/.config/ble2influx/sensors.json", "Sensors descriptor file")
		logfile             = flag.String("log", "", "Path to log file. stdout if unused")
		influx_only_connect = flag.Bool("influx-only-connect", false, "Connect InfluxDB without pushing metrics")
		period              = flag.Int("period", 60, "Duration (in sec) between two influxdB metrics updates")
	*/

	mijiaConfig = make([]MijiaDeviceConfig, 0)
	client      influxdb2.Client
	writeAPI    influxdb2api.WriteAPIBlocking
	// Map for latest measurement and its Mutex
	lastMetrics = make(map[[6]byte]*MijiaMetrics)
	lockMetrics = sync.RWMutex{}
	lastUpload  = time.Now()
)

/*
 * decodeMijia decodes BLE adv payload
 */
func decodeMijia(dat []byte) (*MijiaMetrics, error) {

	if len(dat) != 15 {
		return nil, errors.New("Bad packet length")
	}

	ret := &MijiaMetrics{}

	for i := 0; i < 6; i++ {
		ret.Mac[i] = dat[5-i]
	}

	// See https://www.fanjoe.be/?p=3911 for negative temperatures
	temp := uint32(dat[7])*0xFF + uint32(dat[6])
	if dat[7] > 0x80 {
		ret.Temp = float32(-65535+int32(temp)) / 100
	} else {
		ret.Temp = float32(temp) / 100
	}
	ret.Humi = float32(uint32(dat[9])*0xFF+uint32(dat[8])) / 100
	ret.Batt = float32(uint32(dat[11])*0xFF+uint32(dat[10])) / 1000
	ret.FrameCount = dat[12]
	return ret, nil
}

// Helper function to drop privileges after we are bind to HCI device
func chuser(username string) (uid, gid int) {
	usr, err := user.Lookup(username)
	if err != nil {
		log.Printf("failed to find user %q: %s\n", username, err)
		os.Exit(3)
	}

	uid, err = strconv.Atoi(usr.Uid)

	if err != nil {
		log.Printf("bad user ID %q: %s\n", usr.Uid, err)
		os.Exit(3)
	}

	gid, err = strconv.Atoi(usr.Gid)

	if err != nil {
		log.Printf("bad group ID %q: %s", usr.Gid, err)
		os.Exit(3)
	}

	if err := syscall.Setgid(gid); err != nil {
		log.Printf("setgid(%d): %s", gid, err)
		os.Exit(3)
	}

	if err := syscall.Setuid(uid); err != nil {
		log.Printf("setuid(%d): %s", uid, err)
		os.Exit(3)
	}

	return uid, gid
}

/*
 * influxSender is a goroutine for sending Mijia metrics to influxdb
 * It assumes InfluxDB connexion is ok
 */
func influxSender(metrics map[[6]byte]*MijiaMetrics, dryRun bool) {

	log.Println("influxSender: Start loop")
	cnt := int(0)
	for {
		cnt = 0
		if dryRun {
			log.Println("Sending influxdb metrics disabled (only_connect) ")
			continue
		}

		log.Println("influxSender: Metrics queue length:", len(metrics))
		for mac, data := range metrics {
			hs := hex.EncodeToString(data.Mac[:])

			// Get name from json config
			dName := "unknown"
			for _, s := range mijiaConfig {
				if s.Mac == hs {
					dName = s.Name
				}
			}

			if config.debugLevel > 0 {
				log.Printf("TX %s: Name:%s Rssi:%d Temp:%.2f Humi:%.2f Batt:%.2f Frame:%d\n", hs, dName, data.RSSI, data.Temp, data.Humi, data.Batt, data.FrameCount)
			}
			p := influxdb2.NewPoint(config.influx_measurement,
				map[string]string{"type": "mijia", "source": hs},
				map[string]interface{}{"rssi": data.RSSI, "temp": data.Temp, "humi": data.Humi, "batt": data.Batt, "name": dName}, time.Now())
			cnt++
			err := writeAPI.WritePoint(context.Background(), p)
			if err != nil {
				log.Println("influxSender: Error TX :", err)
			}
			lockMetrics.Lock()
			delete(metrics, mac)
			lockMetrics.Unlock()
		}
		//writeAPI.Flush()
		lastUpload = time.Now()

		log.Println("influxSender: Sleep")
		time.Sleep(time.Duration(config.period) * time.Second)
		log.Println("influxSender: Sent ", cnt, " measurements, now sleeping ", config.period, "sec.")
	}
}

func chkErr(err error) {
	switch errors.Cause(err) {
	case nil:
	case context.DeadlineExceeded:
		log.Printf("done\n")
	case context.Canceled:
		log.Printf("canceled\n")
	default:
		log.Fatalf(err.Error())
	}
}

/*
 * advHandler processes BLE ads
 */
func advHandler(a ble.Advertisement) {

	// Dump received frame
	if config.debugLevel > 1 {
		log.Println("vvvvv-------------------")
		log.Printf("  Found device: %s\n", a.Addr())
		log.Printf("  Local Name: %s\n", a.LocalName())
		log.Printf("  RSSI: %d\n", a.RSSI())
		log.Printf("  Manufacturer Data: %x\n", a.ManufacturerData())
		log.Printf("  Service UUIDs: %v\n\n", a.Services())
		log.Printf("  Service DATA: %v\n\n", a.ServiceData())
		log.Println("~~~~~-------------------")
		s := spew.Sdump(a)
		log.Println(s)
		log.Println("^^^^^-------------------")

	}
	if strings.HasPrefix(a.LocalName(), "ATC_") && len(a.ServiceData()) > 0 {
		for _, svc := range a.ServiceData() {

			// Discard if it's not Mijia (0x181a)
			if !svc.UUID.Equal(ble.UUID16(0x181a)) {
				if config.debugLevel > 2 {
					log.Println("Skipping UUID", svc.UUID)
				}
				continue
			}

			// Try to decode payload
			mi, err := decodeMijia(svc.Data)
			if err == nil {
				hs := hex.EncodeToString(mi.Mac[:])
				if config.debugLevel > 0 {
					log.Printf("RX: MIJIA: %s: Rssi:%d Temp:%.2f Humi:%.2f Batt:%.2f Frame:%d\n", hs, a.RSSI(), mi.Temp, mi.Humi, mi.Batt, mi.FrameCount)
				}
				mi.RSSI = a.RSSI()
				lockMetrics.Lock()
				lastMetrics[mi.Mac] = mi
				lockMetrics.Unlock()

			} else {
				log.Println("Error decoding frame:", err)
			}

		}

	}
}

func parseConfig(path string) error {
	// Load config file
	cfg, err := ini.Load(path)
	if err != nil {
		return err
	}

	config.debugLevel, err = cfg.Section("").Key("debuglevel").Int()
	if err != nil {
		println("Error parsing debugLevel")
		return err
	}
	config.period, err = cfg.Section("").Key("period").Int()
	if err != nil {
		println("Error parsing period")
		return err
	}
	config.logFile = cfg.Section("").Key("logFile").String()
	config.device = cfg.Section("").Key("device").String()

	config.influx_url = cfg.Section("influx").Key("url").String()
	config.influx_bucket = cfg.Section("influx").Key("bucket").String()
	config.influx_measurement = cfg.Section("influx").Key("measurement").String()
	config.influx_org = cfg.Section("influx").Key("organization").String()
	config.influx_token = cfg.Section("influx").Key("apikey").String()

	return nil
}

// MAIN
func main() {

	log.Println("Starting ble2influx")

	// Get CLI parameters
	configFile = flag.String("config", ".ble2influx.conf", "Configuration file")
	dryrun = flag.Bool("device", false, "Dry Run (listen BT but don't connect influxDB)")
	flag.Parse()

	println("  configFile: ", *configFile)
	println("  dryrun: ", *dryrun)

	// Read configuration file, exit if not found
	err := parseConfig(*configFile)
	if err != nil {
		fmt.Println(err)
		os.Exit(0)
	}

	// Set log file
	if len(config.logFile) > 0 {
		log.Println("Log file set to: ", config.logFile)
		f, err := os.OpenFile(config.logFile, os.O_RDWR|os.O_CREATE|os.O_APPEND, 0600)
		if err == nil {
			log.SetOutput(f)
		} else {
			log.Fatal(err)
		}
	}

	log.Println("Creating BLE device")
	d, err := dev.NewDevice("")
	if err != nil {
		log.Fatalf("can't new device : %s", err)
	}

	//log.Println("Switching to ", *dropuser, " user")
	//chuser(*dropuser)

	/*
		// Try to read default
		descFile := *sensors_descriptor
		if strings.HasPrefix(descFile, "~/") {
			dirname, _ := os.UserHomeDir()
			descFile = filepath.Join(dirname, descFile[2:])
		}
		log.Println("Trying to load sensor descriptor: ", descFile)
		if _, err := os.Stat(descFile); err == nil {
			log.Println("Using json sensor descriptor file ", descFile)

			dat, err := os.ReadFile(descFile)
			if err != nil {
				log.Fatalf("Can't read %s", descFile)
			}

			// Try to read default json sensors configuration
			log.Println("Trying to load sensor descriptor: ", *sensors_descriptor)
			if _, err := os.Stat(*sensors_descriptor); err == nil {
				log.Println("Using json sensor descriptor file ", sensors_descriptor)

				// Mijia configuration json
				err = json.Unmarshal(dat, &mijiaConfig)
				if err != nil {
					log.Fatalf("Can't parse json file :", err)
				}
				log.Println("Mijia json configuration : ", len(mijiaConfig), " entries found")
			} else {
				log.Println("No sensor descriptor json ", descFile, " ", err)
			}
		}

	*/
	//InfluxDB connection
	log.Println("Connecting to influxDB server")
	log.Println("  - server:", config.influx_url, ",bucket:", config.influx_bucket)
	log.Println("  - org:", config.influx_org, ",measurement:", config.influx_measurement)
	log.Println("  - apikey:", config.influx_token)
	ble.SetDefaultDevice(d)
	client = influxdb2.NewClient(config.influx_url, config.influx_token)
	defer client.Close()
	writeAPI = client.WriteAPIBlocking(config.influx_org, config.influx_bucket)

	// Run routine for sending Mijia metrics
	go influxSender(lastMetrics, *dryrun)

	// Scan forever, or until interrupted by user.
	log.Println("Starting BLE Advertisement Listener")
	ctx := ble.WithSigHandler(context.Background(), nil)
	chkErr(ble.Scan(ctx, true, advHandler, nil))
}
