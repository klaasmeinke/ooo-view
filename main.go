package main

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"runtime"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/zalando/go-keyring"
	"golang.org/x/oauth2"
	"golang.org/x/oauth2/google"
	"google.golang.org/api/calendar/v3"
	"google.golang.org/api/option"
)

const (
	serviceName     = "calendar-ui"
	clientSecretKey = "client-secret"
	tokenKey        = "oauth-token"
)

type Config struct {
	WeeksAhead  int
	MinDuration time.Duration
	TimeZone    string
}

func parseFlags() Config {
	// Get system's local timezone
	localTZ, err := time.LoadLocation("Local")
	if err != nil {
		log.Fatalf("Error getting local timezone: %v", err)
	}

	cfg := Config{
		WeeksAhead:  8,
		MinDuration: 24 * time.Hour,
		TimeZone:    localTZ.String(), // Use system's local timezone
	}

	flag.IntVar(&cfg.WeeksAhead, "weeks", cfg.WeeksAhead, "Number of weeks ahead to check")
	flag.DurationVar(&cfg.MinDuration, "min-duration", cfg.MinDuration, "Minimum duration of out-of-office events to show (e.g., 24h, 48h, 72h)")
	flag.StringVar(&cfg.TimeZone, "timezone", cfg.TimeZone, "Time zone for calendar display")
	resetSecret := flag.Bool("reset-secret", false, "Reset stored client secret")
	resetToken := flag.Bool("reset-token", false, "Reset stored OAuth token")
	flag.Parse()

	// Handle reset flags
	if *resetSecret {
		if err := keyring.Delete(serviceName, clientSecretKey); err != nil {
			log.Printf("Warning: Could not delete client secret: %v", err)
		} else {
			fmt.Println("Client secret has been reset.")
		}
		// Also reset the token when client secret is reset
		if err := keyring.Delete(serviceName, tokenKey); err != nil {
			log.Printf("Warning: Could not delete OAuth token: %v", err)
		} else {
			fmt.Println("OAuth token has been reset.")
		}
	}

	if *resetToken {
		if err := keyring.Delete(serviceName, tokenKey); err != nil {
			log.Printf("Warning: Could not delete OAuth token: %v", err)
		} else {
			fmt.Println("OAuth token has been reset.")
		}
	}

	// Override with environment variable if set
	if tz := os.Getenv("CALENDAR_TIMEZONE"); tz != "" {
		cfg.TimeZone = tz
	}

	return cfg
}

func getConfig(ctx context.Context) (*oauth2.Config, error) {
	// Try to get client secret from keyring
	clientSecret, err := keyring.Get(serviceName, clientSecretKey)
	if err != nil {
		fmt.Println("First time setup. Please provide your Google OAuth client secret:")
		fmt.Println("1. Go to https://console.cloud.google.com")
		fmt.Println("2. Create a new project or select an existing one")
		fmt.Println("3. Enable the Google Calendar API")
		fmt.Println("4. Go to Credentials and create an OAuth 2.0 Client ID, with http://127.0.0.1 as the redirect URI")
		fmt.Println("6. Download the client secret JSON file")
		fmt.Println("\nPaste the contents of your client_secret.json file and press Enter:")

		// Create a channel to receive the input
		inputChan := make(chan string)
		errChan := make(chan error)

		// Start a goroutine to read input
		go func() {
			scanner := bufio.NewScanner(os.Stdin)
			if scanner.Scan() {
				inputChan <- scanner.Text()
			}
			if err := scanner.Err(); err != nil {
				errChan <- err
			}
		}()

		// Wait for either input or context cancellation
		select {
		case secret := <-inputChan:
			// Validate that the input is valid JSON
			var jsonCheck map[string]interface{}
			if err := json.Unmarshal([]byte(secret), &jsonCheck); err != nil {
				return nil, fmt.Errorf("invalid JSON format: %v\nPlease make sure you're pasting the entire client_secret.json file", err)
			}

			// Try to create config to validate it's a proper client secret
			if _, err := google.ConfigFromJSON([]byte(secret), calendar.CalendarReadonlyScope); err != nil {
				return nil, fmt.Errorf("invalid client secret format: %v\nPlease make sure you're using the correct client_secret.json file", err)
			}

			// Store the secret
			err = keyring.Set(serviceName, clientSecretKey, secret)
			if err != nil {
				return nil, fmt.Errorf("failed to store client secret: %v", err)
			}
			clientSecret = secret
		case err := <-errChan:
			return nil, fmt.Errorf("error reading input: %v", err)
		case <-ctx.Done():
			return nil, fmt.Errorf("operation cancelled")
		}
	}

	config, err := google.ConfigFromJSON([]byte(clientSecret), calendar.CalendarReadonlyScope)
	if err != nil {
		return nil, fmt.Errorf("unable to parse client secret: %v", err)
	}
	return config, nil
}

func generateRandomState() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.URLEncoding.EncodeToString(b), nil
}

func getToken(ctx context.Context, config *oauth2.Config) (*oauth2.Token, error) {
	// Generate random state parameter
	state, err := generateRandomState()
	if err != nil {
		return nil, fmt.Errorf("unable to generate state parameter: %v", err)
	}

	// Try to get token from keyring
	tokenJSON, err := keyring.Get(serviceName, tokenKey)
	if err == nil {
		var token oauth2.Token
		if err := json.Unmarshal([]byte(tokenJSON), &token); err == nil {
			// Check if token is expired
			if token.Expiry.After(time.Now()) {
				return &token, nil
			}
		}
	}

	// Create a channel to receive the auth code
	codeChan := make(chan string)
	errChan := make(chan error)

	// Get a random port
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, fmt.Errorf("unable to get random port: %v", err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	listener.Close()

	// Update config with the correct redirect URI
	config.RedirectURL = fmt.Sprintf("http://127.0.0.1:%d", port)

	// Create a server with a custom handler
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		// Verify state parameter
		if r.URL.Query().Get("state") != state {
			errChan <- fmt.Errorf("invalid state parameter")
			return
		}

		code := r.URL.Query().Get("code")
		if code == "" {
			errChan <- fmt.Errorf("no code received")
			return
		}
		codeChan <- code
		w.Write([]byte("Authorization successful! You can close this window."))
	})

	server := &http.Server{
		Addr:    fmt.Sprintf("127.0.0.1:%d", port),
		Handler: http.TimeoutHandler(mux, 30*time.Second, "Request timeout"),
	}

	// Start server in a goroutine
	go func() {
		if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			errChan <- fmt.Errorf("server error: %v", err)
		}
	}()

	// Generate auth URL and open browser
	authURL := config.AuthCodeURL(state, oauth2.AccessTypeOffline)
	fmt.Printf("Opening browser for authorization...\n")
	if err := openBrowser(authURL); err != nil {
		return nil, fmt.Errorf("unable to open browser: %v", err)
	}

	// Wait for auth code or context cancellation
	var authCode string
	select {
	case authCode = <-codeChan:
		fmt.Println("Authorization code received, exchanging for token...")
	case err := <-errChan:
		return nil, err
	case <-ctx.Done():
		return nil, ctx.Err()
	}

	// Exchange code for token
	tok, err := config.Exchange(ctx, authCode)
	if err != nil {
		return nil, fmt.Errorf("unable to retrieve token from web: %v", err)
	}
	fmt.Println("Token received successfully!")

	// Save token to keyring
	tokenBytes, err := json.Marshal(tok)
	if err != nil {
		return nil, fmt.Errorf("unable to marshal token: %v", err)
	}
	if err := keyring.Set(serviceName, tokenKey, string(tokenBytes)); err != nil {
		return nil, fmt.Errorf("unable to store token: %v", err)
	}

	// Shutdown server in background
	go func() {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		server.Shutdown(shutdownCtx)
	}()

	return tok, nil
}

func openBrowser(url string) error {
	var err error
	switch runtime.GOOS {
	case "linux":
		err = exec.Command("xdg-open", url).Start()
	case "windows":
		err = exec.Command("rundll32", "url.dll,FileProtocolHandler", url).Start()
	case "darwin":
		err = exec.Command("open", url).Start()
	default:
		err = fmt.Errorf("unsupported platform")
	}
	return err
}

func getGroupFreebusy(ctx context.Context, srv *calendar.Service, groupEmail string, timeMin, timeMax time.Time, timezone string) (map[string]calendar.FreeBusyCalendar, error) {
	body := &calendar.FreeBusyRequest{
		TimeMin:  timeMin.Format(time.RFC3339),
		TimeMax:  timeMax.Format(time.RFC3339),
		TimeZone: timezone,
		Items: []*calendar.FreeBusyRequestItem{
			{Id: groupEmail},
		},
		GroupExpansionMax:    100,
		CalendarExpansionMax: 50,
	}

	resp, err := srv.Freebusy.Query(body).Context(ctx).Do()
	if err != nil {
		if strings.Contains(err.Error(), "Not Found") {
			return nil, fmt.Errorf("group '%s' not found or you don't have access to it. Please check if the email address is correct", groupEmail)
		}
		return nil, fmt.Errorf("unable to query freebusy: %v", err)
	}

	if len(resp.Calendars) == 0 {
		return nil, fmt.Errorf("no calendars found for group '%s'. You might not have access to view the group's calendars", groupEmail)
	}

	return resp.Calendars, nil
}

type CalendarEvent struct {
	Start   time.Time
	End     time.Time
	Summary string
	Person  string
}

func displayCalendar(eventsByPerson map[string][]*calendar.Event, timeMin, timeMax time.Time) {
	// Create a map to store all events by date
	eventsByDate := make(map[string]map[string]bool) // date -> person -> hasOOO

	// Group events by date
	for person, events := range eventsByPerson {
		for _, event := range events {
			var start, end time.Time
			var err error

			if event.Start.DateTime != "" {
				start, err = time.Parse(time.RFC3339, event.Start.DateTime)
			} else {
				start, err = time.Parse("2006-01-02", event.Start.Date)
			}
			if err != nil {
				continue
			}

			if event.End.DateTime != "" {
				end, err = time.Parse(time.RFC3339, event.End.DateTime)
			} else {
				end, err = time.Parse("2006-01-02", event.End.Date)
			}
			if err != nil {
				continue
			}

			// Add event to each day it spans
			for d := start; d.Before(end); d = d.AddDate(0, 0, 1) {
				dateKey := d.Format("2006-01-02")
				if eventsByDate[dateKey] == nil {
					eventsByDate[dateKey] = make(map[string]bool)
				}
				eventsByDate[dateKey][person] = true
			}
		}
	}

	// Get the first day of the week for the start date
	startDate := timeMin
	for startDate.Weekday() != time.Monday {
		startDate = startDate.AddDate(0, 0, -1)
	}

	// Print calendar by weeks
	currentDate := startDate
	for currentDate.Before(timeMax) || currentDate.Equal(timeMax) {
		// Print week header
		weekEnd := currentDate.AddDate(0, 0, 6)
		fmt.Println()
		fmt.Printf("%-20s | Mon | Tue | Wed | Thu | Fri | Sat | Sun |\n",
			fmt.Sprintf("%s %d - %s %d",
				currentDate.Format("Jan"),
				currentDate.Day(),
				weekEnd.Format("Jan"),
				weekEnd.Day()))
		fmt.Println("----------------------------------------------------------------")

		// Get people with OOO events this week
		peopleThisWeek := make(map[string]bool)
		for i := 0; i < 7; i++ {
			dateKey := currentDate.AddDate(0, 0, i).Format("2006-01-02")
			for person := range eventsByDate[dateKey] {
				peopleThisWeek[person] = true
			}
		}

		// Sort people alphabetically
		people := make([]string, 0, len(peopleThisWeek))
		for person := range peopleThisWeek {
			people = append(people, person)
		}
		sort.Strings(people)

		// Print each person's row or "No OOO Events" if empty
		if len(people) == 0 {
			fmt.Println("No OOO Events")
		} else {
			for _, person := range people {
				displayName := person
				if len(person) > 20 {
					displayName = person[:17] + "..."
				}
				fmt.Printf("%-20s |", displayName)
				for i := 0; i < 7; i++ {
					dateKey := currentDate.AddDate(0, 0, i).Format("2006-01-02")
					if eventsByDate[dateKey][person] {
						fmt.Print(" OOO |")
					} else {
						fmt.Print("     |")
					}
				}
				fmt.Println()
			}
		}
		fmt.Println("----------------------------------------------------------------")

		// Move to next week
		currentDate = currentDate.AddDate(0, 0, 7)
	}

	fmt.Println()
}

func getOutOfOfficeEvents(ctx context.Context, srv *calendar.Service, calendarId string, timeMin, timeMax time.Time, minDuration time.Duration, timezone string) ([]*calendar.Event, error) {
	events, err := srv.Events.List(calendarId).
		TimeMin(timeMin.Format(time.RFC3339)).
		TimeMax(timeMax.Format(time.RFC3339)).
		SingleEvents(true).
		EventTypes("outOfOffice").
		OrderBy("startTime").
		Context(ctx).
		Do()
	if err != nil {
		return nil, fmt.Errorf("unable to retrieve events: %v", err)
	}

	// Load the configured timezone
	loc, err := time.LoadLocation(timezone)
	if err != nil {
		return nil, fmt.Errorf("invalid timezone: %v", err)
	}

	// Filter events by minimum duration
	var filteredEvents []*calendar.Event
	for _, event := range events.Items {
		var start, end time.Time
		var err error

		if event.Start.DateTime != "" {
			start, err = time.Parse(time.RFC3339, event.Start.DateTime)
		} else {
			// For all-day events, use the start of the day in the configured timezone
			start, err = time.ParseInLocation("2006-01-02", event.Start.Date, loc)
		}
		if err != nil {
			continue
		}

		if event.End.DateTime != "" {
			end, err = time.Parse(time.RFC3339, event.End.DateTime)
		} else {
			// For all-day events, use the start of the day in the configured timezone
			end, err = time.ParseInLocation("2006-01-02", event.End.Date, loc)
		}
		if err != nil {
			continue
		}

		if end.Sub(start) >= minDuration {
			filteredEvents = append(filteredEvents, event)
		}
	}

	return filteredEvents, nil
}

func main() {
	cfg := parseFlags()

	// Create a context that can be cancelled
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Handle Ctrl+C
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, os.Interrupt, syscall.SIGTERM)
	go func() {
		<-sigChan
		fmt.Println("\nShutting down gracefully...")
		cancel()
	}()

	// Get group email from command line arguments
	args := flag.Args()
	if len(args) != 1 {
		fmt.Println("Error: Missing group email address")
		fmt.Println("\nUsage:")
		fmt.Println("  go run main.go [options] <group-email>")
		fmt.Println("\nOptions:")
		fmt.Println("  --weeks N         Number of weeks ahead to check")
		fmt.Println("  --min-duration D  Minimum duration (e.g., 24h, 48h, 72h)")
		fmt.Println("  --timezone TZ     Time zone for calendar display")
		fmt.Println("  --reset-secret    Reset stored client secret")
		fmt.Println("  --reset-token     Reset stored OAuth token")
		fmt.Println("\nExample:")
		fmt.Println("  go run main.go --weeks 8 group-id@example.com")
		os.Exit(1)
	}
	groupEmail := args[0]

	oauthConfig, err := getConfig(ctx)
	if err != nil {
		log.Fatalf("Error getting config: %v", err)
	}

	tok, err := getToken(ctx, oauthConfig)
	if err != nil {
		log.Fatalf("Error getting token: %v", err)
	}

	// Create Calendar service
	calService, err := calendar.NewService(ctx, option.WithTokenSource(oauthConfig.TokenSource(ctx, tok)))
	if err != nil {
		log.Fatalf("Error creating calendar service: %v", err)
	}

	// Get the start of the current week (Monday)
	now := time.Now().UTC()
	for now.Weekday() != time.Monday {
		now = now.AddDate(0, 0, -1)
	}
	now = time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location())

	// Calculate end date to include the full last week
	end := now.AddDate(0, 0, cfg.WeeksAhead*7)
	// Move to the end of the last week (Sunday)
	for end.Weekday() != time.Sunday {
		end = end.AddDate(0, 0, 1)
	}
	end = time.Date(end.Year(), end.Month(), end.Day(), 23, 59, 59, 0, end.Location())

	// Get free/busy information
	calendars, err := getGroupFreebusy(ctx, calService, groupEmail, now, end, cfg.TimeZone)
	if err != nil {
		log.Fatalf("Error: %v", err)
	}

	// Collect all events by person
	eventsByPerson := make(map[string][]*calendar.Event)
	var mu sync.Mutex
	var wg sync.WaitGroup

	for userEmail := range calendars {
		wg.Add(1)
		go func(email string) {
			defer wg.Done()
			events, err := getOutOfOfficeEvents(ctx, calService, email, now, end, cfg.MinDuration, cfg.TimeZone)
			if err != nil {
				log.Printf("Error getting OOO events for %s: %v", email, err)
				return
			}
			mu.Lock()
			eventsByPerson[email] = events
			mu.Unlock()
		}(userEmail)
	}

	wg.Wait()

	// Display combined calendar view
	displayCalendar(eventsByPerson, now, end)
}
