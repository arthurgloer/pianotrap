package main

import (
    "bufio"
    "fmt"
    "os"
    "os/exec"
    "os/signal"
    "path/filepath"
    "regexp"
    "strings"
    "sync"
    "syscall"
    "time"

    "github.com/creack/pty"
    "golang.org/x/term"
)

// Constants for configuration and timing
const (
    defaultSaveDir    = "~/Music"
    configSubDir      = ".config/pianotrap"
    configFileName    = "config"
    pianobarConfigDir = ".config/pianobar"
    eventCmdFileName  = "eventcmd.sh"
    silenceThreshold  = 15 * time.Second
    minRecordTime     = 30 * time.Second
    timeThreshold     = 5 * time.Second // Allow 5s leeway for "complete" songs
)

// Config holds the configuration settings
type Config struct {
    SaveDir string
}

// ResourceManager tracks recording resources and handles cleanup
type ResourceManager struct {
    mu            sync.Mutex
    ffmpegCmd     *exec.Cmd
    currentFile   string
    recording     bool
    remainingTime time.Duration
    totalDuration time.Duration
}

func NewResourceManager() *ResourceManager {
    return &ResourceManager{}
}

func (rm *ResourceManager) StartRecording(cmd *exec.Cmd, file string) {
    rm.mu.Lock()
    defer rm.mu.Unlock()
    rm.ffmpegCmd = cmd
    rm.currentFile = file
    rm.recording = true
}

func (rm *ResourceManager) Cleanup() {
    rm.mu.Lock()
    defer rm.mu.Unlock()
    if rm.ffmpegCmd != nil && rm.recording {
        fmt.Printf("\r\nStopping recording process")
        rm.ffmpegCmd.Process.Signal(syscall.SIGTERM)
        time.Sleep(500 * time.Millisecond)
        if err := rm.ffmpegCmd.Process.Kill(); err != nil {
            fmt.Fprintf(os.Stderr, "\r\nWarning: failed to kill ffmpeg: %v\n", err)
        }
        rm.recording = false
        rm.ffmpegCmd = nil
    }
    if rm.currentFile != "" {
        fmt.Printf("\r\nRemoving incomplete file: %s\n", rm.currentFile)
        if err := os.Remove(rm.currentFile); err != nil {
            fmt.Fprintf(os.Stderr, "\r\nError removing file %s: %v\n", rm.currentFile, err)
        }
        rm.currentFile = ""
    }
    rm.remainingTime = 0
    rm.totalDuration = 0
}

// LoadConfig loads or creates the configuration file
func LoadConfig() (Config, error) {
    homeDir, err := os.UserHomeDir()
    if err != nil {
        return Config{}, fmt.Errorf("could not get home directory: %v", err)
    }

    configDir := filepath.Join(homeDir, configSubDir)
    configPath := filepath.Join(configDir, configFileName)

    if _, err := os.Stat(configPath); os.IsNotExist(err) {
        if err := os.MkdirAll(configDir, 0755); err != nil {
            return Config{}, fmt.Errorf("error creating config directory: %v", err)
        }
        defaultSaveDir := filepath.Join(homeDir, "Music")
        if err := os.WriteFile(configPath, []byte(defaultSaveDir+"\n"), 0644); err != nil {
            return Config{}, fmt.Errorf("error creating default config file: %v", err)
        }
        fmt.Printf("\r\nCreated default config file at %s with save directory: %s\n", configPath, defaultSaveDir)
    } else if err != nil {
        return Config{}, fmt.Errorf("error checking config file: %v", err)
    }

    file, err := os.Open(configPath)
    if err != nil {
        return Config{}, fmt.Errorf("error opening config file: %v", err)
    }
    defer file.Close()

    scanner := bufio.NewScanner(file)
    if scanner.Scan() {
        saveDir := strings.TrimSpace(scanner.Text())
        if saveDir != "" {
            return Config{SaveDir: saveDir}, nil
        }
    }
    return Config{SaveDir: filepath.Join(homeDir, "Music")}, nil
}

// setupPianobarEventCmd configures Pianobar's event command script
func setupPianobarEventCmd() error {
    homeDir, _ := os.UserHomeDir()
    pianobarConfigDirPath := filepath.Join(homeDir, pianobarConfigDir)
    pianobarConfigPath := filepath.Join(pianobarConfigDirPath, "config")
    eventCmdPath := filepath.Join(pianobarConfigDirPath, eventCmdFileName)

    if _, err := os.Stat(eventCmdPath); os.IsNotExist(err) {
        if err := os.MkdirAll(pianobarConfigDirPath, 0755); err != nil {
            return fmt.Errorf("error creating pianobar config directory: %v", err)
        }
        eventCmdContent := `#!/bin/bash
echo "EVENT: $1" >> /tmp/pianobar_event.log
echo "SONGNAME: $pianobar_songname" >> /tmp/pianobar_event.log
echo "ARTIST: $pianobar_artist" >> /tmp/pianobar_event.log
echo "STATION: $pianobar_stationName" >> /tmp/pianobar_event.log
if [ "$1" = "songstart" ]; then
    echo "SONGSTART: $pianobar_songname by $pianobar_artist on $pianobar_stationName"
fi
if [ "$1" = "songfinish" ]; then
    echo "SONGFINISH"
fi`
        if err := os.WriteFile(eventCmdPath, []byte(eventCmdContent), 0755); err != nil {
            return fmt.Errorf("error creating eventcmd script: %v", err)
        }
        fmt.Printf("\r\nCreated eventcmd script at %s\n", eventCmdPath)
    }

    configContent := fmt.Sprintf("event_command = %s\n", eventCmdPath)
    if _, err := os.Stat(pianobarConfigPath); os.IsNotExist(err) {
        if err := os.WriteFile(pianobarConfigPath, []byte(configContent), 0644); err != nil {
            return fmt.Errorf("error creating pianobar config: %v", err)
        }
        fmt.Printf("\r\nCreated pianobar config at %s\n", pianobarConfigPath)
    } else {
        file, err := os.ReadFile(pianobarConfigPath)
        if err != nil {
            return fmt.Errorf("error reading pianobar config: %v", err)
        }
        if !strings.Contains(string(file), "event_command") {
            f, err := os.OpenFile(pianobarConfigPath, os.O_APPEND|os.O_WRONLY, 0644)
            if err != nil {
                return fmt.Errorf("error opening pianobar config for append: %v", err)
            }
            defer f.Close()
            if _, err := f.WriteString(configContent); err != nil {
                return fmt.Errorf("error appending to pianobar config: %v", err)
            }
            fmt.Printf("\r\nAppended event_command to %s\n", pianobarConfigPath)
        }
    }
    return nil
}

// getPulseMonitorSource retrieves the PulseAudio monitor source
func getPulseMonitorSource() (string, error) {
    cmd := exec.Command("pactl", "get-default-sink")
    output, err := cmd.Output()
    if err != nil {
        return "", fmt.Errorf("error getting default sink: %v", err)
    }
    sinkName := strings.TrimSpace(string(output))

    cmd = exec.Command("pactl", "list", "sources")
    output, err = cmd.Output()
    if err != nil {
        return "", fmt.Errorf("error listing sources: %v", err)
    }

    scanner := bufio.NewScanner(strings.NewReader(string(output)))
    for scanner.Scan() {
        line := scanner.Text()
        if strings.Contains(line, "Name:") && strings.Contains(line, ".monitor") {
            sourceName := strings.TrimSpace(strings.Split(line, "Name:")[1])
            if strings.HasPrefix(sourceName, sinkName) {
                return sourceName, nil
            }
        }
    }
    return "", fmt.Errorf("no monitor source found for default sink %s", sinkName)
}

// parseTime converts "MM:SS" to time.Duration
func parseTime(timeStr string) (time.Duration, error) {
    parts := strings.Split(timeStr, ":")
    if len(parts) != 2 {
        return 0, fmt.Errorf("invalid time format: %s", timeStr)
    }
    minutes, err := time.ParseDuration(parts[0] + "m")
    if err != nil {
        return 0, fmt.Errorf("invalid minutes: %s", parts[0])
    }
    seconds, err := time.ParseDuration(parts[1] + "s")
    if err != nil {
        return 0, fmt.Errorf("invalid seconds: %s", parts[1])
    }
    return minutes + seconds, nil
}

// splitByNewlines splits input on \r or \n for proper line handling
func splitByNewlines(data []byte, atEOF bool) (advance int, token []byte, err error) {
    if atEOF && len(data) == 0 {
        return 0, nil, nil
    }
    for i := 0; i < len(data); i++ {
        if data[i] == '\r' || data[i] == '\n' {
            if i > 0 {
                return i + 1, data[:i], nil
            }
            return i + 1, nil, nil
        }
    }
    if atEOF {
        return len(data), data, nil
    }
    return 0, nil, nil
}

// stripANSI removes ANSI escape codes from strings
func stripANSI(s string) string {
    re := regexp.MustCompile(`\033\[[0-9;]*[a-zA-Z]`)
    return re.ReplaceAllString(s, "")
}

func cleanLine(line string, width int) string {
    line = strings.TrimSpace(line) // Remove leading/trailing spaces
    if line == "" {
        return "" // Skip empty lines
    }
    // Pad to the specified width (80 characters)
    if len(line) < width {
        line += strings.Repeat(" ", width-len(line))
    }
    return line
}

// RunPianotrap is the main function to run the Pianobar trap
func RunPianotrap(cfg Config) error {
    monitorSource, err := getPulseMonitorSource()
    if err != nil {
        fmt.Fprintf(os.Stderr, "\r\nWarning: could not determine PulseAudio monitor source: %v\nFalling back to 'default.monitor'\n", err)
        monitorSource = "default.monitor"
    }
    fmt.Printf("\r\nUsing PulseAudio monitor source: %s\n", monitorSource)

    // Start Pianobar in a PTY
    pianobarCmd := exec.Command("pianobar")
    ptyFile, err := pty.Start(pianobarCmd)
    if err != nil {
        return fmt.Errorf("error starting pianobar in PTY: %v", err)
    }

    // Resource manager for cleanup
    rm := NewResourceManager()
    defer rm.Cleanup() // Ensure cleanup on function exit
    defer ptyFile.Close()

    _, err2 := term.GetState(int(os.Stdin.Fd()))
    if err2 != nil {
        fmt.Fprintf(os.Stderr, "\r\nWarning: could not save terminal state: %v\n", err2)
    }

    // Set terminal to raw mode
    oldState, err := term.MakeRaw(int(os.Stdin.Fd()))
    if err != nil {
        fmt.Fprintf(os.Stderr, "\r\nWarning: could not set terminal to raw mode: %v\n", err)
    } else {
        defer term.Restore(int(os.Stdin.Fd()), oldState)
    }

    // Send 'i' to toggle song info
    go func() {
        time.Sleep(5 * time.Second)
        if _, err := ptyFile.Write([]byte("i\n")); err != nil {
            fmt.Fprintf(os.Stderr, "\r\nError sending 'i' to pianobar: %v\n", err)
        }
    }()

    // Channel to signal Pianobar exit
    done := make(chan struct{})
    var closeOnce sync.Once
    closeDone := func() {
        closeOnce.Do(func() {
            close(done)
        })
    }

    go func() {
        if err := pianobarCmd.Wait(); err != nil {
            fmt.Fprintf(os.Stderr, "\r\nPianobar exited with error: %v\n", err)
        }
        closeDone()
    }()

    // Shutdown channel for graceful exit
    shutdown := make(chan struct{})
    inputDone := make(chan struct{})
    go func() {
        defer close(inputDone)
        buf := make([]byte, 1024)
        for {
            select {
            case <-done:
                return
            case <-shutdown:
                return
            default:
                n, err := os.Stdin.Read(buf)
                if err != nil {
                    if err.Error() != "EOF" {
                        fmt.Fprintf(os.Stderr, "\r\n\r\nError reading from stdin: %v\n", err)
                    }
                    return
                }
                fmt.Fprintf(os.Stderr, "\r\nSending to PTY: %q\n", string(buf[:n]))
                if _, err := ptyFile.Write(buf[:n]); err != nil {
                    fmt.Fprintf(os.Stderr, "\r\nError writing to PTY: %v\n", err)
                    return
                }
                if strings.Contains(string(buf[:n]), "q") {
                    close(shutdown) // Trigger graceful shutdown
                    return
                }
            }
        }
    }()

    // Handle signals
    sigChan := make(chan os.Signal, 1)
    signal.Notify(sigChan, os.Interrupt, syscall.SIGTERM)
    go func() {
        <-sigChan
        close(shutdown) // Trigger graceful shutdown
    }()

    // Regex for countdown timer
    countdownRe := regexp.MustCompile(`#\s+-(?:(\d+):)?(\d+):(\d+)/(\d+):(\d+)`)

    scanner := bufio.NewScanner(ptyFile)
    scanner.Split(splitByNewlines)

    var currentStation string
    var lastSong string
    var lastLine string
    var inCountdown bool
loop:
    for {
        select {
        case <-done:
            if inCountdown {
                fmt.Printf("\r\n")
            }
            break loop
        case <-shutdown:
            rm.Cleanup() // Clean up resources
            pianobarCmd.Process.Signal(syscall.SIGTERM)
            time.Sleep(500 * time.Millisecond)
            pianobarCmd.Process.Kill()
            ptyFile.Close()
            closeDone()
            break loop
        default:
            if !scanner.Scan() {
                if err := scanner.Err(); err != nil {
                    if err.Error() != "read /dev/ptmx: input/output error" {
                        fmt.Fprintf(os.Stderr, "\r\nError reading PTY output: %v\n", err)
                    }
                    closeDone()
                }
                break loop
            }
            line := stripANSI(scanner.Text())
            if line == "" {
                continue
            }

            line = cleanLine(line, 80)
            if line == "" {
                continue
            }

            // Handle countdown lines
            isCountdown := strings.HasPrefix(line, "#")
            if isCountdown {
                if !inCountdown && lastLine != "" {
                    fmt.Printf("\r\n")
                }
                fmt.Printf("\r%s", line)
                inCountdown = true
            } else {
                if inCountdown {
                    fmt.Printf("\r\n")
                    inCountdown = false
                }
                cleaned := cleanLine(line, 80)
                fmt.Printf("\r\n%s", cleaned)
            }
            lastLine = line

            // Station change handling
            stationRe := regexp.MustCompile(`\|\>\s*Station\s+"([^"]+)"`)
            if matches := stationRe.FindStringSubmatch(line); matches != nil {
                newStation := sanitizeFileName(matches[1])
                fmt.Fprintf(os.Stderr, "\r\nStation detected: %s\n", newStation)
                if newStation != currentStation {
                    rm.Cleanup()
                    currentStation = newStation
                    stationDir := filepath.Join(cfg.SaveDir, currentStation)
                    if err := os.MkdirAll(stationDir, 0755); err != nil {
                        fmt.Fprintf(os.Stderr, "\r\nFailed to create station dir %s: %v\n", stationDir, err)
                    } else {
                        fmt.Printf("\r\nCreated station directory: %s\n", stationDir)
                    }
                    fmt.Printf("\r\nSwitched to station: %s\n", currentStation)
                }
                continue
            }

            // Parse countdown timer
            if matches := countdownRe.FindStringSubmatch(line); matches != nil {
                remainingStr := fmt.Sprintf("%s:%s", matches[2], matches[3])
                if matches[1] != "" {
                    remainingStr = fmt.Sprintf("%s:%s", matches[1], matches[2])
                }
                totalStr := fmt.Sprintf("%s:%s", matches[4], matches[5])
                remaining, err := parseTime(remainingStr)
                if err != nil {
                    fmt.Fprintf(os.Stderr, "\r\nError parsing remaining time: %v\n", err)
                    continue
                }
                total, err := parseTime(totalStr)
                if err != nil {
                    fmt.Fprintf(os.Stderr, "\r\nError parsing total time: %v\n", err)
                    continue
                }
                rm.mu.Lock()
                rm.remainingTime = remaining
                rm.totalDuration = total
                rm.mu.Unlock()
            }

            // Song change handling
            songRe := regexp.MustCompile(`\|\>\s*"([^"]+)"\s*by\s*"([^"]+)"\s*on\s*"([^"]+)"`)
            if matches := songRe.FindStringSubmatch(line); matches != nil {
                songTitle := matches[1]
                artist := matches[2]
                currentSong := line
                if currentSong != lastSong {
                    rm.mu.Lock()
                    deleteFile := rm.recording && rm.totalDuration > 0 && rm.remainingTime > timeThreshold
                    rm.mu.Unlock()
                    if deleteFile {
                        rm.Cleanup()
                    }
                    if currentStation == "" {
                        currentStation = "Unknown Station"
                    }
                    fmt.Fprintf(os.Stderr, "\r\nSaving with station: %s\n", currentStation)
                    currentFileName := filepath.Join(cfg.SaveDir, currentStation, sanitizeFileName(fmt.Sprintf("%s - %s.mp3", songTitle, artist)))
                    fmt.Printf("\r\nSong detected - Starting to save: %s\n", currentFileName)
                    ffmpegCmd := exec.Command("ffmpeg", "-f", "pulse", "-i", monitorSource, "-af", "volume=2", "-acodec", "mp3", "-y", currentFileName)
                    rm.StartRecording(ffmpegCmd, currentFileName)
                    go saveSong(cfg, ffmpegCmd, currentFileName, monitorSource, rm)
                    lastSong = currentSong
                }
            }

            if strings.HasPrefix(line, "SONGFINISH") && rm.recording {
                fmt.Printf("\r\nSong finished, stopping capture")
                rm.mu.Lock()
                rm.recording = false
                rm.mu.Unlock()
                lastSong = ""
            }

            if strings.Contains(line, "(i) Network error") || strings.Contains(line, "Connection lost") {
                rm.Cleanup()
                lastSong = ""
            }
        }
    }

    <-inputDone
    return nil
}

// sanitizeFileName cleans up filenames
func sanitizeFileName(name string) string {
    re := regexp.MustCompile(`\033\[[0-9;]*[a-zA-Z]|\|>|\s*"`)
    clean := re.ReplaceAllString(name, "")
    clean = strings.ReplaceAll(clean, "/", "-")
    clean = strings.ReplaceAll(clean, ":", "-")
    clean = strings.ReplaceAll(clean, "*", "")
    clean = strings.ReplaceAll(clean, "?", "")
    return strings.TrimSpace(clean)
}

// saveSong records audio using ffmpeg
func saveSong(cfg Config, cmd *exec.Cmd, fileName string, monitorSource string, rm *ResourceManager) {
    if err := os.MkdirAll(filepath.Dir(fileName), 0755); err != nil {
        fmt.Fprintf(os.Stderr, "\r\nError creating station dir for song %s: %v\n", fileName, err)
        return
    }

    if err := exec.Command("pactl", "set-sink-volume", "@DEFAULT_SINK@", "100%").Run(); err != nil {
        fmt.Fprintf(os.Stderr, "\r\nError setting volume: %v\n", err)
    }

    cmd.Stdout = nil
    errPipe, _ := cmd.StderrPipe()
    errScanner := bufio.NewScanner(errPipe)
    startTime := time.Now()

    fmt.Printf("\r\nStarting ffmpeg for %s\n", fileName)
    if err := cmd.Start(); err != nil {
        fmt.Fprintf(os.Stderr, "\r\nError starting ffmpeg: %v\n", err)
        return
    }

    go func() {
        ticker := time.NewTicker(silenceThreshold)
        defer ticker.Stop()
        var lastSize int64
        silenceCount := 0
        for range ticker.C {
            rm.mu.Lock()
            if cmd != rm.ffmpegCmd || !rm.recording {
                rm.mu.Unlock()
                return
            }
            if time.Since(startTime) < minRecordTime {
                rm.mu.Unlock()
                continue
            }
            rm.mu.Unlock()
            if info, err := os.Stat(fileName); err == nil {
                currentSize := info.Size()
                if currentSize == lastSize && currentSize > 1024*50 {
                    silenceCount++
                    if silenceCount >= 2 {
                        rm.mu.Lock()
                        if cmd == rm.ffmpegCmd && rm.recording {
                            fmt.Printf("\r\nDetected prolonged silence (possible sleep), stopping recording")
                            rm.Cleanup()
                        }
                        rm.mu.Unlock()
                        return
                    }
                } else {
                    silenceCount = 0
                }
                lastSize = currentSize
            }
        }
    }()

    go func() {
        var errOutput strings.Builder
        for errScanner.Scan() {
            errOutput.WriteString(errScanner.Text() + "\n")
        }
        rm.mu.Lock()
        defer rm.mu.Unlock()
        if cmd != rm.ffmpegCmd {
            return
        }
        if err := cmd.Wait(); err != nil && !strings.Contains(err.Error(), "signal") {
            fmt.Fprintf(os.Stderr, "\r\nError capturing audio for %s: %v\n%s", fileName, err, errOutput.String())
            os.Remove(fileName)
            return
        }
        if info, err := os.Stat(fileName); err == nil {
            fmt.Printf("\r\nSaved: %s (%d bytes)\n", fileName, info.Size())
            if info.Size() < 1024*50 {
                fmt.Printf("\r\nSkipping incomplete track: %s\n", fileName)
                os.Remove(fileName)
            }
        } else {
            fmt.Fprintf(os.Stderr, "\r\nFile not found after capture for %s: %v\n", fileName, err)
        }
        rm.recording = false
    }()
}

// main initializes and runs the program
func main() {
    cfg, err := LoadConfig()
    if err != nil {
        fmt.Fprintf(os.Stderr, "\r\nError loading config: %v\n", err)
        os.Exit(1)
    }

    if err := os.MkdirAll(cfg.SaveDir, 0755); err != nil {
        fmt.Fprintf(os.Stderr, "\r\nError creating save directory: %v\n", err)
        os.Exit(1)
    }

    if err := setupPianobarEventCmd(); err != nil {
        fmt.Fprintf(os.Stderr, "\r\nError setting up pianobar eventcmd: %v\n", err)
        os.Exit(1)
    }

    fmt.Printf("\r\nSaving songs to: %s\n", cfg.SaveDir)

    if err := RunPianotrap(cfg); err != nil {
        fmt.Fprintf(os.Stderr, "\r\nError running Pianotrap: %v\n", err)
        os.Exit(1)
    }
}
