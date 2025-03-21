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

const (
    defaultSaveDir    = "~/Music"
    configSubDir      = ".config/pianotrap"
    configFileName    = "config"
    pianobarConfigDir = ".config/pianobar"
    eventCmdFileName  = "eventcmd.sh"
    silenceThreshold  = 15 * time.Second
    minRecordTime     = 30 * time.Second
    timeThreshold     = 5 * time.Second
)

type Config struct {
    SaveDir string
}

var (
    currentStation  string
    currentFileName string
    ffmpegCmd       *exec.Cmd
    recording       bool
    remainingTime   time.Duration
    totalDuration   time.Duration
    mu              sync.Mutex
    pianobarSinkID  string
)

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

func stopRecording(deleteFile bool) {
    mu.Lock()
    defer mu.Unlock()
    if ffmpegCmd != nil && recording {
        fmt.Printf("\r\nStopping current recording\n")
        ffmpegCmd.Process.Signal(syscall.SIGTERM)
        time.Sleep(500 * time.Millisecond)
        if err := ffmpegCmd.Process.Kill(); err != nil {
            fmt.Fprintf(os.Stderr, "\r\nWarning: failed to kill ffmpeg: %v\n", err)
        }
        if deleteFile && currentFileName != "" {
            fmt.Printf("\r\nRemoving incomplete file: %s\n", currentFileName)
            os.Remove(currentFileName)
        }
        recording = false
        ffmpegCmd = nil
    }
    remainingTime = 0
    totalDuration = 0
}

func setupPianobarSink() (string, error) {
    // Get the default sink (speakers)
    cmd := exec.Command("pactl", "get-default-sink")
    defaultSink, err := cmd.Output()
    if err != nil {
        return "", fmt.Errorf("failed to get default sink: %v", err)
    }
    defaultSinkName := strings.TrimSpace(string(defaultSink))
    fmt.Printf("\r\nDefault sink (speakers): %s\n", defaultSinkName)

    // Create a null sink for recording Pianobar audio
    cmd = exec.Command("pactl", "load-module", "module-null-sink", "sink_name=PianobarSink", "sink_properties=device.description=PianobarSink")
    output, err := cmd.Output()
    if err != nil {
        return "", fmt.Errorf("failed to create PianobarSink: %v", err)
    }
    pianobarSinkID = strings.TrimSpace(string(output))
    fmt.Printf("\r\nCreated PianobarSink with module ID: %s\n", pianobarSinkID)

    // Wait for Pianobar to start (called later in RunPianotrap)
    // We'll move its sink input to the default sink and loop it to PianobarSink after Pianobar is running

    // Set up a loopback from default sink monitor to PianobarSink
    cmd = exec.Command("pactl", "load-module", "module-loopback", "sink=PianobarSink", "source="+defaultSinkName+".monitor")
    loopbackID, err := cmd.Output()
    if err != nil {
        return "", fmt.Errorf("failed to create loopback to PianobarSink: %v", err)
    }
    fmt.Printf("\r\nCreated loopback with ID %s from %s.monitor to PianobarSink\n", strings.TrimSpace(string(loopbackID)), defaultSinkName)

    // Ensure PianobarSink is unmuted and audible
    cmd = exec.Command("pactl", "set-sink-mute", "PianobarSink", "0")
    if err := cmd.Run(); err != nil {
        fmt.Fprintf(os.Stderr, "\r\nWarning: failed to unmute PianobarSink: %v\n", err)
    }
    cmd = exec.Command("pactl", "set-sink-volume", "PianobarSink", "100%")
    if err := cmd.Run(); err != nil {
        fmt.Fprintf(os.Stderr, "\r\nWarning: failed to set PianobarSink volume: %v\n", err)
    }

    return "PianobarSink.monitor", nil
}

func cleanupPianobarSink() {
    if pianobarSinkID != "" {
        cmd := exec.Command("pactl", "unload-module", pianobarSinkID)
        if err := cmd.Run(); err != nil {
            fmt.Fprintf(os.Stderr, "\r\nWarning: failed to unload PianobarSink module %s: %v\n", pianobarSinkID, err)
        } else {
            fmt.Printf("\r\nUnloaded PianobarSink module %s\n", pianobarSinkID)
        }
    }
}

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

func stripANSI(s string) string {
    re := regexp.MustCompile(`\033\[[0-9;]*[a-zA-Z]`)
    return re.ReplaceAllString(s, "")
}

func cleanLine(line string, width int) string {
    line = strings.TrimSpace(line)
    if line == "" {
        return ""
    }
    if len(line) < width {
        line += strings.Repeat(" ", width-len(line))
    }
    return line
}

func RunPianotrap(cfg Config) error {
    monitorSource := "PianobarSink.monitor"
    fmt.Printf("\r\nUsing PulseAudio monitor source: %s\n", monitorSource)

    pianobarCmd := exec.Command("./launch_pianobar.sh")
    ptyFile, err := pty.Start(pianobarCmd)
    if err != nil {
        return fmt.Errorf("error starting pianobar script in PTY: %v", err)
    }
    defer ptyFile.Close()

    oldState, err := term.MakeRaw(int(os.Stdin.Fd()))
    if err != nil {
        fmt.Fprintf(os.Stderr, "\r\nWarning: could not set terminal to raw mode: %v\n", err)
    } else {
        defer term.Restore(int(os.Stdin.Fd()), oldState)
    }

    go func() {
        time.Sleep(5 * time.Second)
        if _, err := ptyFile.Write([]byte("i\n")); err != nil {
            fmt.Fprintf(os.Stderr, "\r\nError sending 'i' to pianobar: %v\n", err)
        }
    }()

    done := make(chan struct{})
    var closeOnce sync.Once
    closeDone := func() {
        closeOnce.Do(func() {
            close(done)
        })
    }

    go func() {
        if err := pianobarCmd.Wait(); err != nil {
            fmt.Fprintf(os.Stderr, "\r\nPianobar script exited with error: %v\n", err)
        }
        closeDone()
    }()

    shutdown := make(chan struct{})
    inputDone := make(chan struct{})

    go func() {
        defer close(inputDone)
        buf := make([]byte, 1)
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
                        fmt.Fprintf(os.Stderr, "\r\nError reading from stdin: %v\n", err)
                    }
                    return
                }
                if n > 0 {
                    fmt.Fprintf(os.Stderr, "\r\nSending to PTY: %q\n", string(buf[:n]))
                    if _, err := ptyFile.Write(buf[:n]); err != nil {
                        fmt.Fprintf(os.Stderr, "\r\nError writing to PTY: %v\n", err)
                        return
                    }
                    if buf[0] == 'q' {
                        stopRecording(true)
                        pianobarCmd.Process.Signal(syscall.SIGTERM)
                        time.Sleep(500 * time.Millisecond)
                        pianobarCmd.Process.Kill()
                        close(shutdown)
                        return
                    }
                }
            }
        }
    }()

    sigChan := make(chan os.Signal, 1)
    signal.Notify(sigChan, os.Interrupt, syscall.SIGTERM)
    go func() {
        <-sigChan
        stopRecording(true)
        pianobarCmd.Process.Signal(syscall.SIGTERM)
        time.Sleep(500 * time.Millisecond)
        pianobarCmd.Process.Kill()
        close(shutdown)
    }()

    // Unbuffered PTY output with full parsing
    go func() {
        buf := make([]byte, 256) // Smaller buffer for faster response
        var lastSong string
        for {
            n, err := ptyFile.Read(buf)
            if err != nil {
                if err.Error() != "read /dev/ptmx: input/output error" {
                    fmt.Fprintf(os.Stderr, "\r\nError reading PTY output: %v\n", err)
                }
                closeDone()
                return
            }
            output := stripANSI(string(buf[:n]))
            if output != "" {
                fmt.Print(output)
                os.Stdout.Sync()

                // Song detection
                songRe := regexp.MustCompile(`\|\>\s*"([^"]+)"\s*by\s*"([^"]+)"\s*on\s*"([^"]+)"`)
                if matches := songRe.FindStringSubmatch(output); matches != nil {
                    songTitle := matches[1]
                    artist := matches[2]
                    currentSong := output
                    if currentSong != lastSong {
                        mu.Lock()
                        deleteFile := recording && totalDuration > 0 && remainingTime > timeThreshold
                        mu.Unlock()
                        stopRecording(deleteFile)
                        if currentStation == "" {
                            currentStation = "Unknown Station"
                        }
                        currentFileName = filepath.Join(cfg.SaveDir, currentStation, sanitizeFileName(fmt.Sprintf("%s - %s.mp3", songTitle, artist)))
                        fmt.Printf("\r\nSong detected - Starting to save: %s\n", currentFileName)
                        ffmpegCmd = exec.Command("ffmpeg", "-f", "pulse", "-i", monitorSource, "-acodec", "mp3", "-y", currentFileName)
                        recording = true
                        go saveSong(cfg, currentFileName, monitorSource)
                        lastSong = currentSong
                    }
                }

                // Station detection
                stationRe := regexp.MustCompile(`\|\>\s*Station\s+"([^"]+)"`)
                if matches := stationRe.FindStringSubmatch(output); matches != nil {
                    newStation := sanitizeFileName(matches[1])
                    fmt.Fprintf(os.Stderr, "\r\nStation detected: %s\n", newStation)
                    if newStation != currentStation {
                        stopRecording(true)
                        currentStation = newStation
                        stationDir := filepath.Join(cfg.SaveDir, currentStation)
                        if err := os.MkdirAll(stationDir, 0755); err != nil {
                            fmt.Fprintf(os.Stderr, "\r\nFailed to create station dir %s: %v\n", stationDir, err)
                        } else {
                            fmt.Printf("\r\nCreated station directory: %s\n", stationDir)
                        }
                        fmt.Printf("\r\nSwitched to station: %s\n", currentStation)
                    }
                }

                // Countdown for song completion
                countdownRe := regexp.MustCompile(`#\s+-(?:(\d+):)?(\d+):(\d+)/(\d+):(\d+)`)
                if matches := countdownRe.FindStringSubmatch(output); matches != nil {
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
                    mu.Lock()
                    remainingTime = remaining
                    totalDuration = total
                    if remaining <= 0 && recording { // Song completed
                        fmt.Printf("\r\nSong finished, stopping capture\n")
                        stopRecording(false)
                    }
                    mu.Unlock()
                }

                // Stop conditions
                if strings.Contains(output, "(i) Network error") || strings.Contains(output, "Connection lost") || strings.Contains(output, "Song paused") {
                    stopRecording(true)
                    lastSong = ""
                }
            }
        }
    }()

loop:
    for {
        select {
        case <-done:
            fmt.Printf("\r\n")
            break loop
        case <-shutdown:
            break loop
        }
    }

    <-inputDone
    return nil
}

func sanitizeFileName(name string) string {
    re := regexp.MustCompile(`\033\[[0-9;]*[a-zA-Z]|\|>|\s*"`)
    clean := re.ReplaceAllString(name, "")
    clean = strings.ReplaceAll(clean, "/", "-")
    clean = strings.ReplaceAll(clean, ":", "-")
    clean = strings.ReplaceAll(clean, "*", "")
    clean = strings.ReplaceAll(clean, "?", "")
    return strings.TrimSpace(clean)
}

func saveSong(cfg Config, fileName string, monitorSource string) {
    if err := os.MkdirAll(filepath.Dir(fileName), 0755); err != nil {
        fmt.Fprintf(os.Stderr, "\r\nError creating station dir for song %s: %v\n", fileName, err)
        return
    }

    mu.Lock()
    cmd := ffmpegCmd
    cmd.Stdout = nil
    errPipe, _ := cmd.StderrPipe()
    errScanner := bufio.NewScanner(errPipe)
    startTime := time.Now()
    mu.Unlock()

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
            mu.Lock()
            if cmd != ffmpegCmd || !recording {
                mu.Unlock()
                return
            }
            if time.Since(startTime) < minRecordTime {
                mu.Unlock()
                continue
            }
            mu.Unlock()
            if info, err := os.Stat(fileName); err == nil {
                currentSize := info.Size()
                if currentSize == lastSize && currentSize > 1024*50 {
                    silenceCount++
                    if silenceCount >= 2 {
                        mu.Lock()
                        if cmd == ffmpegCmd && recording {
                            fmt.Printf("\r\nDetected prolonged silence (possible sleep), stopping recording\n")
                            stopRecording(true)
                        }
                        mu.Unlock()
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
        mu.Lock()
        defer mu.Unlock()
        if cmd != ffmpegCmd {
            return
        }
        if err := cmd.Wait(); err != nil && !strings.Contains(err.Error(), "signal") {
            fmt.Fprintf(os.Stderr, "\r\nError capturing audio for %s: %v\n%s\n", fileName, err, errOutput.String())
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
        recording = false
    }()
}

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
