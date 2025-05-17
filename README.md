# ooo-events

View a calendar of Google Group OOO events from your CLI.

```
% ooo-events --weeks 2 team@example.com

May 12 - May 18      | Mon | Tue | Wed | Thu | Fri | Sat | Sun |
----------------------------------------------------------------
danny@example.com    |     |     |     | OOO |     |     |     |
jenny@example.com    |     | OOO |     |     |     |     |     |
----------------------------------------------------------------

May 19 - May 25      | Mon | Tue | Wed | Thu | Fri | Sat | Sun |
----------------------------------------------------------------
No OOO Events
----------------------------------------------------------------
```

`ooo-events` is an easy CLI tool that provides a clear, consolidated view of out-of-office (OOO) events for members of a Google group. It fetches data directly from the Google Calendar API and displays it in an easy-to-read weekly format in your terminal.

Ideal for teams to quickly see who is away without needing to manually check multiple calendars.

## Features

- Customizable time range (default: 8 weeks)
- Configurable minimum duration for OOO events
- Timezone support
- Secure credential storage using system keyring
- Beautiful terminal output

## Prerequisites

* **Go**: Version 1.18 or later installed.
* **Google Cloud Project**: You'll need an OAuth 2.0 Client ID from a Google Cloud Project with the Google Calendar API enabled. The tool will guide you through the specifics if it can't find a stored client secret.

## Installation

### Using Go Install

```bash
go install github.com/klaasmeinke/ooo-events@latest
```

### From Source

```bash
# Clone the repository
git clone https://github.com/klaasmeinke/ooo-events.git
cd ooo-events

# Build the project
go build

# Install globally
go install
```

## Usage

Basic usage:
```bash
ooo-events <group-email>
```

Options:
```bash
--weeks N         Number of weeks ahead to check (default: 8)
--min-duration D  Minimum duration of OOO events (e.g., 24h, 48h, 72h)
--timezone TZ     Time zone for calendar display
--reset-secret    Reset stored client secret
--reset-token     Reset stored OAuth token
```

Examples:
```bash
# View OOO events for the next 2 weeks
ooo-events --weeks 2 team@example.com

# Only show OOO events that are at least 48 hours long
ooo-events --min-duration 48h team@example.com

# Use a specific timezone
ooo-events --timezone "America/New_York" team@example.com
```

## Configuration

The tool stores your Google OAuth credentials securely using your system's keyring. You can reset these credentials using the `--reset-secret` and `--reset-token` flags.

## Contributing

Contributions are welcome! Please feel free to submit a Pull Request.

## License

This project is licensed under the MIT License - see the [LICENSE](LICENSE) file for details.

