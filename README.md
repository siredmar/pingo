# pingo

`pingo` is a ping monitor with notification capabilities over Signal.

## Configuration

Configure the `config.yaml`, example:

```yaml
interval: 10s
timeout: 2s
count: 3

# Flap control
failure_threshold: 3      # 3 consecutive failures → DOWN
recovery_threshold: 3     # 3 consecutive successes → UP

# Hosts to monitor
targets:
  - 1.1.1.1
  - 8.8.8.8
  - nas.local
  - 192.168.1.1

signal:
  api_url: http://signal:8080
  from_number: "+49XXXXXXXXXX"     # number registered inside the signal container
  recipients:
    - "+49YYYYYYYYYY"              # numbers and/or group IDs
  prefix_down: "⚠️ DOWN"
  prefix_up: "✅ UP"
```

## Running

Pingo can read an env variable `CONFIG` that defines the location of the config file. If not specified it will look at a `config.yaml` in the current working directory.
Spin up the docker-compose file that starts pingo and an signal-api server to send the notifications.

```bash
$ docker-compose up -d
```

## Bootstrapping Signal Notifications

For pingo to be able to send notifications, the signal-api container needs to be bootstrapped first.

```bash
$ curl http://127.0.0.1:8085/v1/qrcodelink?device_name=pingo -o pingo.png
```

Use the QR code to couple a new device. This allows pingo to send signal notifications on your behalf.
