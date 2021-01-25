package yrsensor

import (
	"encoding/json"
	"fmt"
	log "github.com/sirupsen/logrus"
	"io/ioutil"
	"net/http"
	"time"
)

// For dependency injection during test:
var (
	Client httpClient
)

func init() {
	Client = &http.Client{
		Timeout: time.Second * 20,
	}
}

// Helper that just run the GET request on a URL.
func request(url string, queryParams map[string]string, userAgent string) (*http.Response, error) {
	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		// This shouldn't happen.
		log.Fatal("(poller) While constructing HTTP request: ", err.Error())
	}
	// Add query params
	reqQuery := req.URL.Query()
	for k, v := range queryParams {
		reqQuery.Add(k, v)
	}
	req.URL.RawQuery = reqQuery.Encode()

	// Set User-Agent
	req.Header.Set("User-Agent", userAgent)

	// Send request
	res, err := Client.Do(req)
	// Pass down the result, note that the caller must check for errors.
	return res, err
}

// Fetches a new forecast and replaces the one we have.
// It takes a location a constructs the URL from the lat/long.
func getNewForecast(loc Location, apiUrl string, apiVersion string, userAgent string) (LocationForecast, error) {
	var forecast LocationForecast

	url := fmt.Sprintf("%s/locationforecast/%s/compact", apiUrl, apiVersion)
	params := map[string]string{
		"lat": fmt.Sprintf("%f", loc.Lat),
		"lon": fmt.Sprintf("%f", loc.Long),
	}
	res, err := request(url, params, userAgent)

	if err != nil {
		log.Errorf("(poller) While getting %s : %s", url, err.Error())
		return forecast, err
	}
	if res.StatusCode != 200 && res.StatusCode != 203 {
		log.Errorf("(poller) Got invalid status %v on %s", res.StatusCode, url)
		return forecast, fmt.Errorf("Invalid status code: %v", res.StatusCode)
	}
	body, err := ioutil.ReadAll(res.Body)
	if err != nil {
		return forecast, err
	}
	err = json.Unmarshal(body, &forecast)
	if err != nil {
		log.Fatalf("error unmarshaling JSON from %s: %s", url, err.Error())
	}
	forecast.Expires, err = http.ParseTime(res.Header.Get("Expires"))
	if err != nil {
		panic("(poller) could not parse expires header")
	}
	return forecast, nil
}

// Transforms the LocationForecast we get from Yr into something minimal we need.
// It basically just scrubs away a lot of stuff we don't need.
func transformForecast(forecast LocationForecast) ObservationTimeSeries {
	var m ObservationTimeSeries
	m.expires = forecast.Expires
	ts := forecast.Properties.Timeseries
	for i := 0; i < len(ts); i++ {
		var err error
		m.ts[i].AirTemperature = ts[i].Data.Instant.Details.AirTemperature
		m.ts[i].AirPressureAtSeaLevel = ts[i].Data.Instant.Details.AirPressureAtSeaLevel
		m.ts[i].Time, err = time.Parse(time.RFC3339, ts[i].Time)
		if err != nil {
			panic("Could not parse time on timeseries")
		}
	}
	log.Debug("(poller) Forecast transformed")
	return m
}

// Checks all the time series and updates them if the data is outdated.
func refreshData(config *PollerConfig) {
	for _, loc := range config.Locations.Locations {
		updateNeeded := config.ObservationCachePtr.observations[loc.Id].expires.Before(time.Now().UTC())

		if updateNeeded {
			log.Debug("(poller) Outdated or no data found. Refreshing ", loc.Id)
			// locking needed?
			log.Debugf("(poller) Current data has expiry %v", config.ObservationCachePtr.observations[loc.Id].expires)
			// No data or invalid data. Refresh the dataset we have.
			forecast, err := getNewForecast(loc, config.ApiUrl, config.ApiVersion, config.UserAgent)
			if err != nil {
				log.Errorf("Got error on forecast: %s. Sleeping 10 sec.", err.Error())
				if config.DaemonStatusPtr != nil {
					config.DaemonStatusPtr.IncPollError(loc.Id, err.Error())
				}
				time.Sleep(10 * time.Second)
				log.Info("(poller) got an error. sleeping a bit.")
			}
			m := transformForecast(forecast)
			config.ObservationCachePtr.observations[loc.Id] = m
			if config.DaemonStatusPtr != nil {
				config.DaemonStatusPtr.IncPoll(loc.Id)
			}
			log.Info("(poller) Observation cache update with new data")
		} else {
			log.Debugf("(poller) %s - current data is up to date.", loc.Id)
		}
	}
}

// Go routine that polls until *control goes false.
func poller(config *PollerConfig) {
	log.Info("Starting poller...")
	for {
		select {
		default:
			refreshData(config)
			select {
			case req := <-config.TsRequestChannel:
				log.Debugf("(poller) got internal req for ts(%s)", req.Location)
				req.ResponseChannel <- config.ObservationCachePtr.observations[req.Location]
			case <-time.After(10 * time.Second):
				log.Debug("(poller) No requests came this time")
			}
		case <-config.Finished:
			log.Info("Poller ending")
			config.Finished <- true
			return
		}
	}
}
