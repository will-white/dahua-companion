package main

import (
	"bufio"
	"context"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"sync/atomic"
	"syscall"
	"time"

	mqtt "github.com/eclipse/paho.mqtt.golang"
	"github.com/icholy/digest"
	"github.com/joho/godotenv"
)

type MqttSettings struct {
	broker   string
	clientid string
	username string
	password string
}

var mqttConnected int32
var httpConnected int32

func main() {
	// Load environment variables from .env file
	username, password, host_or_ip, MqttSettings := getEnvironmentVariables()

	mqttClient := setupMQTT(MqttSettings)

	httpClient := &http.Client{
		Transport: &digest.Transport{
			Username: username,
			Password: password,
		},
	}

	server := setupHealthCheck(httpClient, host_or_ip)

	// Channel to signal shutdown
	shutdown := make(chan struct{})

	go func() {
		for {
			select {
			case <-shutdown:
				return
			default:
				listenToMQTT(httpClient, host_or_ip, mqttClient)
			}
		}
	}()

	// Handle graceful shutdown
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)

	<-sigChan
	log.Println("Shutting down...")

	// Close MQTT connection
	mqttClient.Disconnect(250)

	// Close HTTP server
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := server.Shutdown(ctx); err != nil {
		log.Fatalf("HTTP server Shutdown: %v", err)
	}

	// Signal the shutdown of the HTTP stream loop
	close(shutdown)

	log.Println("Shutdown complete")
}

func listenToMQTT(httpClient *http.Client, host_or_ip string, mqttClient mqtt.Client) {
	url := fmt.Sprintf("http://%s/cgi-bin/eventManager.cgi?action=attach&codes=[AlarmLocal]&heartbeat=30", host_or_ip)
	resp, err := httpClient.Get(url)
	if err != nil {
		log.Printf("Error fetching http stream: %v\n", err)
		atomic.StoreInt32(&httpConnected, 0)
		time.Sleep(5 * time.Second) // Wait before retrying
		return
	}

	if resp.StatusCode != http.StatusOK {
		log.Printf("Received non-OK HTTP status: %d\n", resp.StatusCode)
		resp.Body.Close()
		atomic.StoreInt32(&httpConnected, 0)
		time.Sleep(5 * time.Second) // Wait before retrying
		return
	}

	log.Print("Connected to HTTP stream and listening for events")

	atomic.StoreInt32(&httpConnected, 1)
	scanner := bufio.NewScanner(resp.Body)
	// Continuously read events from the stream
	for scanner.Scan() {
		bytes := scanner.Bytes()

		/**
		*	We get a line of bytes and I'm unsure if it's possible to stream it rather than wait for all the bytes
		*	But the string we're looking for is: Code=AlarmLocal;action=Start;index=0
		*	And since its static / doesn't change we can just look for it's length and the first character that doesn't
		*	conflict with the "Stop" of the alarm. i.e. "a" == 97
		 */
		if len(bytes) == 36 && bytes[25] == 97 {
			log.Println("Doorbell pressed")
			if token := mqttClient.Publish("doorbell/pressed", 0, false, ""); token.Wait() && token.Error() != nil {
				log.Println(token.Error())
			}
		}
	}

	if err := scanner.Err(); err != nil {
		log.Printf("Error reading the stream: %v\n", err)
	}

	resp.Body.Close()
	time.Sleep(5 * time.Second)
}

func setupHealthCheck(httpClient *http.Client, host_or_ip string) *http.Server {
	server := &http.Server{Addr: ":8080"}
	http.HandleFunc("/health", healthCheckHandler(httpClient, host_or_ip))
	go func() {
		log.Print("HTTP server started on 8080")

		if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("Could not listen on :8080: %v\n", err)
		}
	}()
	return server
}

func setupMQTT(settings MqttSettings) mqtt.Client {
	opts := mqtt.NewClientOptions()
	opts.AddBroker(settings.broker)
	opts.SetClientID(settings.clientid)
	opts.SetUsername(settings.username)
	opts.SetPassword(settings.password)
	opts.OnConnect = connectHandler
	opts.OnConnectionLost = connectLostHandler
	mqttClient := mqtt.NewClient(opts)
	if token := mqttClient.Connect(); token.Wait() && token.Error() != nil {
		panic(token.Error())
	}
	return mqttClient
}

func getEnvironmentVariables() (string, string, string, MqttSettings) {
	godotenv.Load(".env.local")
	godotenv.Load()

	username := os.Getenv("USERNAME")
	if username == "" {
		log.Fatal("USERNAME environment variable is required")
	}

	password := os.Getenv("PASSWORD")
	if password == "" {
		log.Fatal("MQTT_BROKER_URL environment variable is required")
	}

	host_or_ip := os.Getenv("HOSTNAME_OR_IP")
	if host_or_ip == "" {
		log.Fatal("HOSTNAME_OR_IP environment variable is required")
	}

	mqttBroker := os.Getenv("MQTT_BROKER_URL")
	if mqttBroker == "" {
		log.Fatal("MQTT_BROKER_URL environment variable is required")
	}

	mqttClient := os.Getenv("MQTT_CLIENT_ID")
	if mqttClient == "" {
		log.Fatal("MQTT_CLIENT_ID environment variable is required")
	}

	mqttUsername := os.Getenv("MQTT_USERNAME")
	if mqttUsername == "" {
		log.Fatal("MQTT_USERNAME environment variable is required")
	}

	mqttPassword := os.Getenv("MQTT_PASSWORD")
	if mqttPassword == "" {
		log.Fatal("MQTT_PASSWORD environment variable is required")
	}

	mqtt := MqttSettings{
		broker:   mqttBroker,
		clientid: mqttClient,
		username: mqttUsername,
		password: mqttPassword,
	}

	return username, password, host_or_ip, mqtt
}

func connectHandler(client mqtt.Client) {
	atomic.StoreInt32(&mqttConnected, 1)
	log.Println("Connected to MQTT broker")
}

func connectLostHandler(client mqtt.Client, err error) {
	atomic.StoreInt32(&mqttConnected, 0)
	log.Printf("Connection lost to MQTT broker: %v", err)
}

func healthCheckHandler(httpClient *http.Client, host_or_ip string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		statusCode := http.StatusOK
		mqttStatus := "connected"
		if atomic.LoadInt32(&mqttConnected) != 1 {
			mqttStatus = "disconnected"
			statusCode = http.StatusServiceUnavailable
		}

		httpStatus := "connected"
		if atomic.LoadInt32(&httpConnected) != 1 {
			httpStatus = "disconnected"
			statusCode = http.StatusServiceUnavailable
		}

		doorbellStatus := "okay"
		url := fmt.Sprintf("http://%s/cgi-bin/configManager.cgi?action=setConfig&VSP_PaaS.Online=true", host_or_ip)
		res, err := httpClient.Get(url)
		if err != nil {
			fmt.Printf("client: error making http request: %s\n", err)
			doorbellStatus = "HTTP Request Error"
		} else if res.StatusCode != http.StatusOK {
			fmt.Printf("Expected 200 response got: %d\n", res.StatusCode)
			doorbellStatus = "HTTP Status Error " + res.Status
			statusCode = http.StatusServiceUnavailable
		}

		if statusCode != http.StatusOK {
			status := fmt.Sprintf("MQTT: %s, HTTP: %s, Doorbell: %s", mqttStatus, httpStatus, doorbellStatus)
			w.Write([]byte(status))
		}

		w.WriteHeader(statusCode)
	}
}
