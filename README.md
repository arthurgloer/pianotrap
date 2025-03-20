# Pianotrap

Pianotrap is a Go application that records songs from
[Pianobar](https://github.com/PromyLOPh/pianobar), a console-based
Pandora client, and saves them as MP3 files. It captures audio output
via PulseAudio, organizes recordings by station, and ensures incomplete
recordings are cleaned up automatically upon exit.

## Features

-   Records songs from Pianobar to MP3 files using ffmpeg.
-   Organizes recordings into station-specific directories (e.g.,
    `~/Music/3 Doors Down Radio/`).
-   Automatically deletes incomplete recordings when the program exits
    (via \'q\' or Ctrl+C).
-   Displays real-time countdown timers and station/song information.
-   Configurable save directory via a configuration file.

## Prerequisites

-   **Go**: Version 1.21 or later (for building the application).
-   **Pianobar**: Installed and configured with a Pandora account.
-   **ffmpeg**: For audio recording and encoding.
-   **PulseAudio**: For capturing system audio output.
-   **Dependencies**:
    -   `github.com/creack/pty`
    -   `golang.org/x/term`

## Installation

1.  **Clone the Repository**:

        git clone https://github.com/arthurgloer/pianotrap.git
        cd pianotrap

2.  **Install Dependencies**:

        go mod init pianotrap
        go get github.com/creack/pty
        go get golang.org/x/term

3.  **Install Required Tools**:
    -   On Ubuntu/Debian:

            sudo apt update
            sudo apt install pianobar ffmpeg pulseaudio-utils

    -   On Fedora:

            sudo dnf install pianobar ffmpeg pulseaudio-libs

    -   Ensure Pianobar is configured (e.g., `~/.config/pianobar/config`
        with your Pandora credentials).

4.  **Build the Application (optional)**:

        go build -o pianotrap pianotrap.go

## Usage

1.  **Run the Program**:

        go run pianotrap.go

    Or, if built:

        ./pianotrap

2.  **Interaction**:
    -   The program starts Pianobar in a PTY and toggles song info
        display (via \'i\' command).
    -   Songs are detected and recorded automatically to
        `~/Music/<Station Name>/<Song Title - Artist>.mp3`.
    -   Press \'q\' and Enter to quit, or use Ctrl+C. Incomplete
        recordings will be deleted.

3.  **Configuration**:
    -   The save directory defaults to `~/Music`. To change it, edit
        `~/.config/pianotrap/config`:

            echo "/path/to/save/dir" > ~/.config/pianotrap/config

## How It Works

-   **Recording**: Uses ffmpeg to capture audio from the PulseAudio
    monitor source, triggered by Pianobar's song output.
-   **Station Detection**: Parses station names (e.g., \"3 Doors Down
    Radio\") and creates directories accordingly.
-   **Cleanup**: A `ResourceManager` ensures that any active recording
    process (ffmpeg) is stopped and incomplete `.mp3` files are deleted
    when the program exits, using a deferred cleanup mechanism.

### Example Output

    Saving songs to: /home/arthur/Music
    Using PulseAudio monitor source: alsa_output.pci-0000_00_1f.3.analog-stereo.monitor
    Welcome to pianobar (2022.04.01-dev)! Press ? for a list of commands.
    (i) Login... Ok.
    (i) Get stations... Ok.
    |>  Station "3 Doors Down Radio" (4526334586650128956)
    Station detected: 3 Doors Down Radio
    Created station directory: /home/arthur/Music/3 Doors Down Radio
    Switched to station: 3 Doors Down Radio
    (i) Receiving new playlist... Ok.
    |>  "Something in the Orange" by "Zach Bryan" on "Something in the Orange"
    Saving with station: 3 Doors Down Radio
    Song detected - Starting to save: /home/arthur/Music/3 Doors Down Radio/Something in the Orange - Zach Bryan.mp3
    Starting ffmpeg for /home/arthur/Music/3 Doors Down Radio/Something in the Orange - Zach Bryan.mp3
    #   -03:46/03:48
    Sending to PTY: "q"
    Stopping recording process
    Removing incomplete file: /home/arthur/Music/3 Doors Down Radio/Something in the Orange - Zach Bryan.mp3
    Pianobar exited with error: signal: terminated

## Notes

-   **Automatic Cleanup**: The program uses a `ResourceManager` with a
    deferred `Cleanup()` call to ensure incomplete recordings are
    removed on exit, avoiding leftover files.
-   **Dependencies**: Ensure ffmpeg and PulseAudio are running. If
    PulseAudio fails, it falls back to \"default.monitor\".
-   **Pianobar Events**: An event command script (`eventcmd.sh`) is set
    up but primarily used for logging; song detection relies on output
    parsing.

## Troubleshooting

-   **No Audio Recorded**: Check PulseAudio (`pactl list sources`) and
    ensure the monitor source is correct.
-   **Files Not Deleted**: Verify filesystem permissions in the save
    directory.
-   **Pianobar Errors**: Ensure Pianobar is configured correctly
    (`~/.config/pianobar/config`).

## Contributing

Feel free to fork the project at
<https://github.com/arthurgloer/pianotrap>, submit issues, or send pull
requests with improvements!

## License

This project is licensed under the [GNU General Public License v2.0
(GPL-2)](https://www.gnu.org/licenses/old-licenses/gpl-2.0.en.html),
inspired by Linus Torvalds' preference for the Linux kernel. See the
[LICENSE](LICENSE) file for details.
