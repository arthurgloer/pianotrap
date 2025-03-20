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

    "golang.org/x/term"
)

const (
    defaultSaveDir    = "~/Music"
    configSubDir      = ".config/pianotrap"
    configFileName    = "config"
    pianobarConfigDir = ".config/pianobar"
    eventCmdFileName  = "eventcmd.sh"
)

type Config struct {
    SaveDir string
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
        fmt.Printf("Created default config file at %s with save directory: %s\n", configPath, defaultSaveDir)
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
        fmt.Printf("Created eventcmd script at %s\n", eventCmdPath)
    }

    configContent := fmt.Sprintf("event_command = %s\n", eventCmdPath)
    if _, err := os.Stat(pianobarConfigPath); os.IsNotExist(err) {
        if err := os.WriteFile(pianobarConfigPath, []byte(configContent), 0644); err != nil {
            return fmt.Errorf("error creating pianobar config: %v", err)
        }
        fmt.Printf("Created pianobar config at %s\n", pianobarConfigPath)
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
            fmt.Printf("Appended event_command to %s\n", pianobarConfigPath)
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

var currentStation string
var currentFileName string
var ffmpegCmd *exec.Cmd
var recording bool
var mu sync.Mutex

func stopRecording(deleteFile bool) {
    mu.Lock()
    defer mu.Unlock()
    if ffmpegCmd != nil && recording {
        fmt.Println("Stopping current recording")
        ffmpegCmd.Process.Kill()
        if deleteFile && currentFileName != "" {
            fmt.Printf("Removing incomplete file: %s\n", currentFileName)
            os.Remove(currentFileName)
        }
        recording = false
        ffmpegCmd = nil
    }
}

func RunPianotrap(cfg Config) error {
    monitorSource, err := getPulseMonitorSource()
    if err != nil {
        fmt.Fprintf(os.Stderr, "Warning: could not determine PulseAudio monitor source: %v\nFalling back to 'default.monitor'\n", err)
        monitorSource = "default.monitor"
    }
    fmt.Printf("Using PulseAudio monitor source: %s\n", monitorSource)

    pianobarCmd := exec.Command("pianobar")
    pianobarOut, err := pianobarCmd.StdoutPipe()
    if err != nil {
        return fmt.Errorf("error setting up pianobar stdout: %v", err)
    }
    pianobarCmd.Stdin = os.Stdin
    pianobarCmd.Stderr = os.Stderr

    termState, err := term.GetState(int(os.Stdin.Fd()))
    if err != nil {
        fmt.Fprintf(os.Stderr, "Warning: could not save terminal state: %v\n", err)
    }

    if err := pianobarCmd.Start(); err != nil {
        return fmt.Errorf("error starting pianobar: %v", err)
    }

    sigChan := make(chan os.Signal, 1)
    signal.Notify(sigChan, os.Interrupt, syscall.SIGTERM)
    go func() {
        <-sigChan
        stopRecording(true) // Delete on Ctrl+C
        pianobarCmd.Process.Kill()
        if termState != nil {
            term.Restore(int(os.Stdin.Fd()), termState)
        }
        os.Exit(1)
    }()

    scanner := bufio.NewScanner(pianobarOut)
    for scanner.Scan() {
        line := scanner.Text()
        fmt.Println(line)

        if strings.Contains(line, "|>  Station \"") {
            stopRecording(true) // Delete on station change
            station := strings.Split(line, "|>  Station \"")[1]
            station = strings.TrimSuffix(station, "\"")
            if idx := strings.Index(station, " ("); idx != -1 {
                station = station[:idx]
            }
            currentStation = sanitizeFileName(station)
            stationDir := filepath.Join(cfg.SaveDir, currentStation)
            if err := os.MkdirAll(stationDir, 0755); err != nil {
                fmt.Fprintf(os.Stderr, "Failed to create station dir %s: %v\n", stationDir, err)
            } else {
                fmt.Printf("Created station directory: %s\n", stationDir)
            }
            fmt.Printf("Switched to station: %s\n", currentStation)
        }

        if strings.Contains(line, "|> ") && strings.Contains(line, " by ") && strings.Contains(line, " on ") {
            stopRecording(false) // Don’t delete on natural song change
            parts := strings.SplitN(line, " by ", 2)
            songTitle := strings.TrimPrefix(parts[0], "|> ")
            artistAndRest := strings.SplitN(parts[1], " on ", 2)
            artist := artistAndRest[0]
            if currentStation == "" {
                currentStation = "Unknown Station"
            }
            currentFileName = filepath.Join(cfg.SaveDir, currentStation, sanitizeFileName(fmt.Sprintf("%s - %s.mp3", songTitle, artist)))
            fmt.Printf("Song detected - Starting to save: %s\n", currentFileName)
            go saveSong(cfg, currentFileName, monitorSource)
            recording = true
        }

        if strings.HasPrefix(line, "SONGFINISH") && ffmpegCmd != nil {
            fmt.Println("Song finished, stopping capture")
            stopRecording(false) // Keep file on natural finish
        }

        // Detect skip ('n') or pause ('p') from user input prompt
        if strings.Contains(line, "[?]") && (strings.Contains(line, "n") || strings.Contains(line, "p")) {
            stopRecording(true) // Delete on skip or pause
        }

        // Detect connection issues
        if strings.Contains(line, "(i) Network error") || strings.Contains(line, "Connection lost") {
            stopRecording(true) // Delete on connection issues
        }
    }

    if err := pianobarCmd.Wait(); err != nil {
        fmt.Fprintf(os.Stderr, "Pianobar exited: %v\n", err)
    }
    stopRecording(true) // Delete on exit (e.g., 'q')
    if termState != nil {
        term.Restore(int(os.Stdin.Fd()), termState)
    }
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
        fmt.Fprintf(os.Stderr, "Error creating station dir for song %s: %v\n", fileName, err)
        return
    }

    if err := exec.Command("pactl", "set-sink-volume", "@DEFAULT_SINK@", "100%").Run(); err != nil {
        fmt.Fprintf(os.Stderr, "Error setting volume: %v\n", err)
    }

    mu.Lock()
    ffmpegCmd = exec.Command("ffmpeg", "-f", "pulse", "-i", monitorSource, "-af", "volume=2", "-acodec", "mp3", "-y", fileName)
    ffmpegCmd.Stdout = nil
    errPipe, _ := ffmpegCmd.StderrPipe()
    errScanner := bufio.NewScanner(errPipe)
    mu.Unlock()

    fmt.Printf("Starting ffmpeg for %s\n", fileName)
    if err := ffmpegCmd.Start(); err != nil {
        fmt.Fprintf(os.Stderr, "Error starting ffmpeg: %v\n", err)
        return
    }

    go func(cmd *exec.Cmd) {
        var errOutput strings.Builder
        for errScanner.Scan() {
            errOutput.WriteString(errScanner.Text() + "\n")
        }
        mu.Lock()
        defer mu.Unlock()
        if cmd != ffmpegCmd { // Command was replaced or killed
            return
        }
        if err := cmd.Wait(); err != nil && !strings.Contains(err.Error(), "signal") {
            fmt.Fprintf(os.Stderr, "Error capturing audio for %s: %v\n%s", fileName, err, errOutput.String())
            os.Remove(fileName)
            return
        }
        if info, err := os.Stat(fileName); err == nil {
            fmt.Printf("Saved: %s (%d bytes)\n", fileName, info.Size())
            if info.Size() < 1024*50 {
                fmt.Printf("Skipping incomplete track: %s\n", fileName)
                os.Remove(fileName)
            }
        } else {
            fmt.Fprintf(os.Stderr, "File not found after capture for %s: %v\n", fileName, err)
        }
        recording = false
    }(ffmpegCmd)
}

func main() {
    cfg, err := LoadConfig()
    if err != nil {
        fmt.Fprintf(os.Stderr, "Error loading config: %v\n", err)
        os.Exit(1)
    }

    if err := os.MkdirAll(cfg.SaveDir, 0755); err != nil {
        fmt.Fprintf(os.Stderr, "Error creating save directory: %v\n", err)
        os.Exit(1)
    }

    if err := setupPianobarEventCmd(); err != nil {
        fmt.Fprintf(os.Stderr, "Error setting up pianobar eventcmd: %v\n", err)
        os.Exit(1)
    }

    fmt.Printf("Saving songs to: %s\n", cfg.SaveDir)

    if err := RunPianotrap(cfg); err != nil {
        fmt.Fprintf(os.Stderr, "Error running Pianotrap: %v\n", err)
        os.Exit(1)
    }
}
