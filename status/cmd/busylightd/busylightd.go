//
// Long-running daemon to control the busylight.
// Automatically polls Google calendar busy/free times
// and can be controlled via signals from a Zoom meeting
// monitoring script:
//
//    USR1   - in zoom, muted
//    USR2   - in zoom, unmuted
//    HUP    - out of zoom
//    INFO   - force refresh from calendar now
//    VTALRM - toggle urgent indicator
//    WINCH  - toggle idle/working state
//    CHLD   - toggle low-priority
//
// Steve Willoughby <steve@madscience.zone>
// License: BSD 3-Clause open-source license
//

package main

import (
	"encoding/json"
	"fmt"
	"io/ioutil"
	"log"
	"net/http"
	"os"
	"os/signal"
	"os/user"
	"path/filepath"
	"regexp"
	"sort"
	"syscall"
	"time"

	"go.bug.st/serial"
	"golang.org/x/net/context"
	"golang.org/x/oauth2"
	"golang.org/x/oauth2/google"
	"google.golang.org/api/calendar/v3"
)

// CalendarConfigData provides configuration data which can be specified for each calendar
// being monitored. These are read from the config.json file.
type CalendarConfigData struct {
	Title              string // Arbitrary user-friendly name for the calendar
	IgnoreAllDayEvents bool   // If true, ignore this calendar if booked the whole time
}

// ConfigData holds the configuration specified by the user in the config.json file
// as well as some run-time values we need to refer to throughout the run of the daemon.
type ConfigData struct {
	// A map of all Google calendars being monitored by the daemon.Calendars
	// The key is the Google-provided calendar ID; the value is a CalendarConfigData
	// structure describing what we want to do with that calendar.
	Calendars map[string]CalendarConfigData

	// The path to the file where our access credentials to the calendars is cached.
	TokenFile string

	// The path to the file where our API keys are stored.
	CredentialFile string

	// The path to our logfile where daemon activity is recorded.
	LogFile string

	// The path to the file where we store our PID while we're running.
	PidFile string

	// The path to the serial device we use to communicate with the light hardware.
	Device string

	// If `Device` is empty, then `DeviceDir` specifies a directory to search for
	// the hardware port. The first file we can successfully open that matches
	// the regular expression `DeviceRegexp` will be used.
	DeviceDir    string
	DeviceRegexp string

	// The baud rate at which we communicate with the hardware.
	BaudRate int

	// These values are used internally by the daemon while it's running.
	googleConfig []byte      // unmarshalled data needed for Google API calls
	logger       *log.Logger // logger open on the requested file
	port         serial.Port // open serial port device
	portOpen     bool        // is `port` valid and open now?
}

// lightSignal tells the hardware to signal a particular condition on the lights.
// If `delay` is positive, we wait that long before returning, to make some trivial
// multi-step (but very quick and short-lived) sequences easy to implement.
func lightSignal(config *ConfigData, color string, delay time.Duration) {
	// colorCode maps the color strings as passed in to this function to the
	// actual commands sent to the hardware.
	var colorCode = map[string]string{
		"blue":     "B",
		"green":    "G",
		"off":      "X",
		"red":      "R",
		"red2":     "2",
		"redflash": "#",
		"urgent":   "%",
		"yellow":   "Y",
		"lowpri":   "@",
	}

	if config.portOpen {
		command, valid := colorCode[color]
		if !valid {
			config.logger.Printf("ERROR: Unable to send light signal \"%v\"; not defined.", color)
			return
		}
		config.port.Write([]byte(command))
		if delay > 0 {
			time.Sleep(delay)
		}
	}
}

func getConfigFromFile(filename string, data *ConfigData) error {
	cdata, err := ioutil.ReadFile(filename)
	if err != nil {
		return fmt.Errorf("Unable to read from %s: %v", filename, err)
	}

	err = json.Unmarshal(cdata, &data)
	if err != nil {
		return fmt.Errorf("Unable to understand %s configuration: %v", filename, err)
	}
	return nil
}

func getClient(config *oauth2.Config, tokFile string) (*http.Client, error) {
	tok, err := tokenFromFile(tokFile)
	if err != nil {
		return nil, err
	}
	return config.Client(context.Background(), tok), nil
}

func tokenFromFile(file string) (*oauth2.Token, error) {
	f, err := os.Open(file)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	tok := &oauth2.Token{}
	err = json.NewDecoder(f).Decode(tok)
	return tok, err
}

// BusyPeriod specifies a range of times during which a calendar indicates one or more events occur.
type BusyPeriod struct {
	Start, End time.Time
}

// ByStartTime provides a custom sort order for `BusyPeriod` elements.
type ByStartTime []BusyPeriod

func (a ByStartTime) Len() int {
	return len(a)
}

func (a ByStartTime) Less(i, j int) bool {
	return a[i].Start.Before(a[j].Start)
}

func (a ByStartTime) Swap(i, j int) {
	a[i], a[j] = a[j], a[i]
}

// CalendarAvailability tracks the overall availability as shown on the monitored calendars.
type CalendarAvailability struct {
	// When did we most recently check with the API to get calendar busy/free times?
	LastPollTime time.Time

	// The list of "busy" time spans found on the calendars from the last poll.
	UpcomingPeriods []BusyPeriod // will be in chronological order
}

// RemoveExpiredPeriods trims busy spans from a `CalendarAvailability` value which occur in the past.
func (cal *CalendarAvailability) RemoveExpiredPeriods(config *ConfigData) {
	for len(cal.UpcomingPeriods) > 0 {
		if time.Now().Add(5 * time.Second).After(cal.UpcomingPeriods[0].End) {
			cal.UpcomingPeriods = cal.UpcomingPeriods[1:]
		} else {
			break
		}
	}
	if len(cal.UpcomingPeriods) == 0 && time.Now().After(cal.LastPollTime.Add(30*time.Minute)) {
		err := cal.Refresh(config)
		if err != nil {
			config.logger.Printf("Unable to refresh calendar data while removing expired periods: %v", err)
		}
	}
	// yes, we're trusting the Google service not to give us past events.
}

// NextTransitionTime returns the absolute time at which we need to check again to change the lights.
func (cal *CalendarAvailability) NextTransitionTime(config *ConfigData) time.Time {
	cal.RemoveExpiredPeriods(config)

	if len(cal.UpcomingPeriods) == 0 {
		// nothing scheduled for the time we queried about.
		// Tell the caller to check back in 8 hours.
		return time.Now().Add(8 * time.Hour)
	}
	if time.Now().Add(5 * time.Second).After(cal.UpcomingPeriods[0].Start) {
		// we're already into the period, so the next transition will be at its end
		return cal.UpcomingPeriods[0].End
	}
	// the period hasn't started yet so the transition will be at its beginning.
	return cal.UpcomingPeriods[0].Start
}

// ScheduledBusyNow checks to see if, according to the monitored calendars, we are scheduled to be busy right now.
func (cal *CalendarAvailability) ScheduledBusyNow(config *ConfigData) bool {
	cal.RemoveExpiredPeriods(config)

	if len(cal.UpcomingPeriods) == 0 {
		return false
	}
	if time.Now().Add(5 * time.Second).After(cal.UpcomingPeriods[0].Start) {
		return true
	}
	return false
}

// Refresh polls the Google API and updates the `CalendarAvailability` structure accordingly.
func (cal *CalendarAvailability) Refresh(config *ConfigData) error {
	config.logger.Printf("Polling Google Calendars")
	googleConfig, err := google.ConfigFromJSON(config.googleConfig, calendar.CalendarReadonlyScope)
	if err != nil {
		return err
	}

	client, err := getClient(googleConfig, config.TokenFile)
	if err != nil {
		return fmt.Errorf("Unable to query calendar: %v", err)
	}

	srv, err := calendar.New(client)
	if err != nil {
		return err
	}

	var query calendar.FreeBusyRequest
	queryStartTime := time.Now()
	queryEndTime := queryStartTime.Add(time.Hour * 8)
	query.TimeMin = queryStartTime.Format(time.RFC3339)
	query.TimeMax = queryEndTime.Format(time.RFC3339)
	for cID := range config.Calendars {
		query.Items = append(query.Items, &calendar.FreeBusyRequestItem{Id: cID})
	}
	freelist, err := srv.Freebusy.Query(&query).Do()
	if err != nil {
		return err
	}

	var rawbusylist []BusyPeriod
	for calID, calData := range freelist.Calendars {
		calInfo, isKnown := config.Calendars[calID]
		if !isKnown {
			config.logger.Printf("WARNING: Calendar <%s> in API results does not match any in our configuration!", calID)
			calInfo = CalendarConfigData{
				Title: fmt.Sprintf("UNKNOWN<%v>", calID),
			}
		}

		for _, e := range calData.Errors {
			config.logger.Printf("ERROR: Calendar \"%s\": %v", calInfo.Title, e)
		}
		for _, busy := range calData.Busy {
			startTime, err := time.Parse(time.RFC3339, busy.Start)
			if err != nil {
				config.logger.Printf("ERROR: %s: Unable to parse start time \"%v\": %v", calInfo.Title, busy.Start, err)
				continue
			}
			endTime, err := time.Parse(time.RFC3339, busy.End)
			if err != nil {
				config.logger.Printf("ERROR: %s: Unable to parse end time \"%v\": %v", calInfo.Title, busy.End, err)
				continue
			}
			config.logger.Printf("Calendar \"%s\": busy %v - %v", calInfo.Title, startTime.Local(), endTime.Local())
			if calInfo.IgnoreAllDayEvents {
				// This calendar is on our ignore list for all-day bookings.
				// There isn't any really great way to identify all-day events
				// since all we see is the aggregate busy time ranges.
				// So we'll compromise by assuming if the calendar is marked busy for the
				// entire query period, it's something we should ignore for the given
				// calendar.
				// It's far from perfect but it gets us closer to something useful.
				if startTime.Before(queryStartTime.Add(5*time.Second)) &&
					endTime.After(queryEndTime.Add(-5*time.Second)) {
					config.logger.Printf("Ignoring long-running event from %s", calInfo.Title)
					continue
				}
			}
			rawbusylist = append(rawbusylist, BusyPeriod{Start: startTime, End: endTime})
		}
	}
	// smush list and sort it
	config.logger.Printf("DEBUG: Initial list: %v", rawbusylist)
	sort.Sort(ByStartTime(rawbusylist))
	config.logger.Printf("DEBUG: Sorted list: %v", rawbusylist)
	var currentStart time.Time
	var currentEnd time.Time

	cal.UpcomingPeriods = nil
	for _, eachPeriod := range rawbusylist {
		if currentEnd.IsZero() {
			currentEnd = eachPeriod.End
		}

		if currentStart.IsZero() {
			currentStart = eachPeriod.Start
		} else if eachPeriod.Start.After(currentEnd) {
			// disjoint; we've reached the end of our busy time, so commit what we have
			cal.UpcomingPeriods = append(cal.UpcomingPeriods, BusyPeriod{Start: currentStart, End: currentEnd})
			currentStart = eachPeriod.Start
			currentEnd = eachPeriod.End
		} else if eachPeriod.End.After(currentEnd) {
			// overlapping; this ends after what we have so far, so extend our busy time
			currentEnd = eachPeriod.End
		} else {
			// overlapping; this is completely inside the time we already have, so we don't need to do anything.
		}
	}
	if !currentStart.IsZero() {
		// we need to commit the last one, too
		cal.UpcomingPeriods = append(cal.UpcomingPeriods, BusyPeriod{Start: currentStart, End: currentEnd})
	}
	config.logger.Printf("DEBUG: final list: %v", cal.UpcomingPeriods)
	cal.LastPollTime = time.Now()
	return nil
}

//
// We maintain a list of busy/free times since the last time we polled the calendar.
// from that we can also know when the next transition time will be
// global state:
//  busy until next transition
//  free until next transition
// Also globally know if in zoom meeting, which overrides the busy/free indicator
//  until the meeting ends.
//
// At transition time:
//  change global state
//  signal status if not in zoom meeting
//  schedule next transition
//
// Hourly:
//  reload state from google
//  update status as it should be now
//  re-schedule next transition

func setup(config *ConfigData) error {
	var thisUser *user.User
	previousLogFile := config.LogFile
	previousPidFile := config.PidFile

	thisUser, err := user.Current()
	if err != nil {
		return fmt.Errorf("Unable to determine current user: %v", err)
	}

	err = getConfigFromFile(filepath.Join(thisUser.HomeDir, ".busylight/config.json"), config)
	if err != nil {
		return fmt.Errorf("Unable to initialize: %v", err)
	}

	//
	// If we're just re-reading the configuration, we will leave the
	// existing logfile and pid file alone.
	//
	if config.logger == nil {
		f, err := os.OpenFile(config.LogFile, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0644)
		if err != nil {
			return fmt.Errorf("Unable to open logfile: %v", err)
		}
		config.logger = log.New(f, "busylightd: ", log.LstdFlags)

		myPID := os.Getpid()
		config.logger.Printf("busylightd started, PID=%v", myPID)

		pidf, err := os.OpenFile(config.PidFile, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0644)
		if err != nil {
			config.logger.Printf("ERROR creating PID file (is another busylightd running?): %v", err)
			return err
		}
		pidf.WriteString(fmt.Sprintf("%d\n", myPID))
		pidf.Close()

		config.googleConfig, err = ioutil.ReadFile(config.CredentialFile)
		if err != nil {
			config.logger.Printf("Unable to read client secret file %v: %v", config.CredentialFile, err)
			return fmt.Errorf("Unable to read client secret file %v: %v", config.CredentialFile, err)
		}
	} else {
		if previousPidFile != config.PidFile {
			config.logger.Printf("WARNING: PID file changed from %v to %v on reload. This requires a full restart of the daemon. Ignoring the change for now.", previousPidFile, config.PidFile)
		}
		if previousLogFile != config.LogFile {
			config.logger.Printf("WARNING: Log file changed from %v to %v on reload. This requires a full restart of the daemon. Ignoring the change for now.", previousLogFile, config.LogFile)
		}
	}

	//
	// Open the hardware port
	//
	if config.portOpen {
		config.port.Close()
		config.portOpen = false
	}

	// If the user had a specific port in mind, just use that.
	if config.Device != "" {
		config.port, err = serial.Open(config.Device, &serial.Mode{
			BaudRate: config.BaudRate,
		})
		if err != nil {
			shutdown(config)
			config.logger.Fatalf("Can't open serial device %v: %v", config.Device, err)
		}
		config.portOpen = true
	} else {
		// On the other hand, maybe we should hunt around to find it.
		// This is necessary on systems where the USB port is given a
		// random device name every time.
		config.logger.Printf("Searching for available device port in %s...", config.DeviceDir)
		fileList, err := os.ReadDir(config.DeviceDir)
		if err != nil {
			shutdown(config)
			config.logger.Fatalf("Can't scan directory %s: %v", config.DeviceDir, err)
		}
		for _, f := range fileList {
			if !f.IsDir() {
				ok, err := regexp.MatchString(config.DeviceRegexp, f.Name())
				if err != nil {
					shutdown(config)
					config.logger.Fatalf("Matching %s vs %s: %v", f.Name(), config.DeviceRegexp, err)
				}
				if ok {
					config.port, err = serial.Open(fmt.Sprintf("%s%c%s", config.DeviceDir, os.PathSeparator, f.Name()),
						&serial.Mode{BaudRate: config.BaudRate})
					if err == nil {
						config.logger.Printf("Opened %s%c%s", config.DeviceDir, os.PathSeparator, f.Name())
						config.portOpen = true
						break
					}
				}
			}
		}
		if !config.portOpen {
			shutdown(config)
			config.logger.Fatalf("Unable to open any device matching /%s/ in %s.", config.DeviceRegexp, config.DeviceDir)
		}
	}

	//
	// Signal that we're online and ready
	//
	lightSignal(config, "blue", 100*time.Millisecond)
	lightSignal(config, "off", 50*time.Millisecond)
	lightSignal(config, "blue", 100*time.Millisecond)
	lightSignal(config, "off", 0)

	return nil
}

//
// reverse whatever setup() did
//
func closeDevice(config *ConfigData) {
	if config.portOpen {
		lightSignal(config, "red2", 100*time.Millisecond)
		lightSignal(config, "off", 50*time.Millisecond)
		lightSignal(config, "red2", 100*time.Millisecond)
		lightSignal(config, "off", 0)
		config.logger.Printf("Closing serial port")
		config.port.Close()
		config.portOpen = false
	}
}

func shutdown(config *ConfigData) {
	closeDevice(config)
	err := os.Remove(config.PidFile)
	if err != nil {
		config.logger.Printf("Error removing PID file: %v", err)
	}
	config.logger.Printf("busylightd shutting down")
}

func main() {
	var config ConfigData

	if err := setup(&config); err != nil {
		log.Fatalf("Unable to start daemon: %v", err)
	}
	defer shutdown(&config)

	//
	// Listen for incoming signals from outside
	//
	req := make(chan os.Signal, 5)
	signal.Notify(req, syscall.SIGHUP, syscall.SIGUSR1, syscall.SIGUSR2, syscall.SIGWINCH, syscall.SIGINFO, syscall.SIGINT, syscall.SIGVTALRM, syscall.SIGCHLD)

	//
	// Get initial calendar download
	//
	var busyTimes CalendarAvailability
	err := busyTimes.Refresh(&config)
	if err != nil {
		config.logger.Printf("Error updating busy/free times from calendar: %v", err)
	}

	isZoomNow := false
	isZoomMuted := false
	isActiveNow := true
	isUrgent := false
	isLowPriority := false

	//
	// Set the current state and schedule for next transition
	//
	isBusyTimeNow := busyTimes.ScheduledBusyNow(&config)
	nextTransitionTime := busyTimes.NextTransitionTime(&config)
	transitionTimer := time.NewTimer(time.Until(nextTransitionTime))

	if isBusyTimeNow {
		lightSignal(&config, "yellow", 0)
	} else {
		lightSignal(&config, "green", 0)
	}

	// We will keep a timer for refreshing the calendar and one for transitioning
	// to the next free/busy state
	refreshTimer := time.NewTicker(time.Hour * 1)

	//
	// Main event loop:
	// 	On incoming signals, indicate light status as requested by signaller
	//  Otherwise, update Google calendar status hourly while active
	//	Update lights based on busy/free status when transition times arrive unless in Zoom
	//
eventLoop:
	for {
		select {
		case _ = <-refreshTimer.C:
			if isActiveNow {
				config.logger.Printf("Periodic calendar refresh starts")
				err = busyTimes.Refresh(&config)
				if err != nil {
					config.logger.Printf("Reload failed: %v", err)
				}
				isBusyTimeNow = busyTimes.ScheduledBusyNow(&config)
				transitionTimer.Stop()
				transitionTimer.Reset(time.Until(busyTimes.NextTransitionTime(&config)))
			} else {
				config.logger.Printf("Ignoring scheduled request to refresh calendar since service isn't active now.")
				refreshTimer.Stop()
			}

		case _ = <-transitionTimer.C:
			config.logger.Printf("Scheduled status change")
			isBusyTimeNow = busyTimes.ScheduledBusyNow(&config)
			transitionTimer.Reset(time.Until(busyTimes.NextTransitionTime(&config)))

		case externalSignal := <-req:
			switch externalSignal {
			case syscall.SIGVTALRM:
				isUrgent = !isUrgent
				config.logger.Printf("Toggle URGENT indicator to %v", isUrgent)

			case syscall.SIGCHLD:
				isLowPriority = !isLowPriority
				config.logger.Printf("Toggle low-priority indicator to %v", isLowPriority)

			case syscall.SIGHUP:
				config.logger.Printf("ZOOM: Call ended")
				isZoomNow = false

			case syscall.SIGUSR1:
				config.logger.Printf("ZOOM: Muted")
				isZoomNow = true
				isZoomMuted = true

			case syscall.SIGUSR2:
				config.logger.Printf("ZOOM: Unmuted")
				isZoomNow = true
				isZoomMuted = false

			case syscall.SIGWINCH:
				config.logger.Printf("Toggle active state")
				isActiveNow = !isActiveNow
				if isActiveNow {
					config.logger.Printf("Activating service; re-loading configuration and opening serial port")
					err = setup(&config)
					if err != nil {
						config.logger.Fatalf("Error loading configuration data. Unable to restart: %v", err)
						return
					}
					config.logger.Printf("Activating service; getting fresh calendar data")
					err = busyTimes.Refresh(&config)
					if err != nil {
						config.logger.Printf("Error updating busy/free times from calendar: %v", err)
					}
					config.logger.Printf("Resetting timers")
					refreshTimer.Reset(1 * time.Hour)
					isBusyTimeNow = busyTimes.ScheduledBusyNow(&config)
					transitionTimer.Reset(time.Until(busyTimes.NextTransitionTime(&config)))
				} else {
					config.logger.Printf("Stopping timers")
					refreshTimer.Stop()
					transitionTimer.Stop()
					closeDevice(&config)
					config.logger.Printf("Daemon in inactive state... zzz")
				}

			case syscall.SIGINFO:
				if isActiveNow {
					config.logger.Printf("Reloading calendar status by request")
					err = busyTimes.Refresh(&config)
					if err != nil {
						config.logger.Printf("Reload failed: %v", err)
					}
					isBusyTimeNow = busyTimes.ScheduledBusyNow(&config)
					transitionTimer.Stop()
					transitionTimer.Reset(time.Until(busyTimes.NextTransitionTime(&config)))
				} else {
					config.logger.Printf("Ignoring reload request since service isn't active now.")
				}

			case syscall.SIGINT:
				config.logger.Printf("Received interrupt signal")
				break eventLoop

			default:
				config.logger.Printf("Received unexpeced signal %v (ignored)", externalSignal)
			}
		}

		// Set signal to current state
		if isActiveNow {
			if isUrgent {
				lightSignal(&config, "urgent", 0)
			} else if isZoomNow {
				if isZoomMuted {
					lightSignal(&config, "red", 0)
					config.logger.Printf("Signal ZOOM MUTED")
				} else {
					lightSignal(&config, "redflash", 0)
					config.logger.Printf("Signal ZOOM OPEN")
				}
			} else if isBusyTimeNow {
				lightSignal(&config, "yellow", 0)
				config.logger.Printf("Signal BUSY")
			} else {
				lightSignal(&config, "green", 0)
				config.logger.Printf("Signal FREE")
			}
			if isLowPriority {
				lightSignal(&config, "lowpri", 0)
			}
		} else {
			lightSignal(&config, "off", 0)
			config.logger.Printf("Signal off")
		}
	}
}
