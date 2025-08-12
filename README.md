
# Project description

xiaomi-ble2influx uses a local hci bluetooth device to listen to Xiaomi Mi Bluetooth broascasts.
It tries to decodes payloads (temperature, humidity, pressure) and periodicaly push the values to influxdb database.


# Compilation

```
git clone https://github.com/ofauchon/xiaomi-ble2influx.git
cd xiaomi-ble2influx
go build
```

## Cross compilation (optional)

In some situation, you may want to compile for another plateform. 
Example to cross-compile for Raspberry PI:
```
GOOS=linux GOARCH=arm GOARM=7 go build -o build/ble2influx-rpi ble2influx.go
```

# Run

ble2influx needs root permissions to open /dev/hci device, but it'll drop to unprivileged if needed

```
$ sudo ./build/ble2influx -user simpleuser
```


# Sensor json descriptor file (optional)

```
[
        {"mac": "a4c1381c1390","model": "xiaomi_mijia","name": "mijia_outside","desc":"Sensor outside"},
        {"mac": "a4c1386b3dc6","model": "xiaomi_mijia","name": "mijia_room1","desc":"Sensor 1"},
        {"mac": "a4c1382b4044","model": "xiaomi_mijia","name": "mijia_room2","desc":"Sensor 2"},
]
```

# Some words about the Mijia protocol

You'll find all the details on the Xiaomi Mijia alternate driver repository:
https://github.com/pvvx/ATC_MiThermometer#bluetooth-advertising-formats
