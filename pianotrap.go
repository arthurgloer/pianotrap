package main

import (
    "context"
    "flag"
    "fmt"
    "log"
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

var (
    mu             sync.Mutex
    recording      bool
    ffmpegCmd      *exec.Cmd
    currentStation string
    currentFileName string
    remainingTime  time.Duration
    totalDuration  time.Duration
    timeThreshold  = 10 * time.Second
    logger         *log.Logger
    logFile        *os.File
    termState      *term.State
)

type Config struct {
    SaveDir string
}

func main() {
    saveDir := flag.String("savedir", "/home/arthur/Music", "directory to save recorded songs")
    logging := flag.Bool("log", false, "enable diagnostic logging to pianotrap.log")
    flag.Parse()

    if *logging {
        var err error
        logFile, err = os.OpenFile("pianotrap.log", os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0644)
        if err != nil {
            fmt.Fprintf(os.Stderr, "Error opening log file: %v\n", err)
            os.Exit(1)
        }
        defer logFile.Close()
        logger = log.New(logFile, "", log.LstdFlags)
    } else {
        logger = log.New(os.Stderr, "", 0)
        logger.SetOutput(os.Stderr)
    }

    cfg := Config{SaveDir: *saveDir}
    fmt.Printf("Saving songs to: %s\n", cfg.SaveDir)
    if err := RunPianotrap(cfg); err != nil {
        logger.Printf("Error running pianotrap: %v", err)
        os.Exit(1)
    }
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

    termState, err = term.MakeRaw(int(os.Stdin.Fd()))
    if err != nil {
        logger.Printf("Warning: could not set terminal to raw mode: %v", err)
    } else {
        defer term.Restore(int(os.Stdin.Fd()), termState)
    }

    go func() {
        time.Sleep(5 * time.Second)
        if _, err := ptyFile.Write([]byte("i\n")); err != nil {
            logger.Printf("Error sending 'i' to pianobar: %v", err)
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
            logger.Printf("Pianobar script exited with error: %v", err)
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
                        logger.Printf("Error reading from stdin: %v", err)
                    }
                    return
                }
                if n > 0 {
                    logger.Printf("Sending to PTY: %q at %v", string(buf[:n]), time.Now())
                    fmt.Printf("%c", buf[0])
                    os.Stdout.Sync()
                    ptyFile.SetWriteDeadline(time.Now().Add(500 * time.Millisecond))
                    if _, err := ptyFile.Write(buf[:n]); err != nil {
                        logger.Printf("Error writing to PTY: %v", err)
                        if os.IsTimeout(err) {
                            logger.Printf("Write timeout, forcing shutdown")
                            stopRecording(true)
                            pianobarCmd.Process.Kill()
                            close(shutdown)
                        }
                        return
                    }
                    ptyFile.SetWriteDeadline(time.Time{})
                    if buf[0] == 'q' {
                        logger.Printf("Quit command received, shutting down")
                        cleanExit(pianobarCmd, 0)
                    }
                }
            }
        }
    }()

    sigChan := make(chan os.Signal, 1)
    signal.Notify(sigChan, os.Interrupt, syscall.SIGTERM)
    go func() {
        <-sigChan
        logger.Printf("SIGTERM received, shutting down")
        cleanExit(pianobarCmd, 0)
    }()

    defer func() {
        exec.Command("pactl", "unload-module", "module-null-sink").Run()
        exec.Command("pactl", "unload-module", "module-loopback").Run()
    }()

    outputChan := make(chan string, 1000)

    go func() {
        buf := make([]byte, 1024)
        var lastSong string
        lastOutputTime := time.Now()
        syscall.SetNonblock(int(ptyFile.Fd()), true)
        defer syscall.SetNonblock(int(ptyFile.Fd()), false)
        for {
            select {
            case <-done:
                return
            case <-shutdown:
                return
            default:
                n, err := ptyFile.Read(buf)
                if err != nil {
                    if errno, ok := err.(syscall.Errno); ok && (errno == syscall.EAGAIN || errno == syscall.EWOULDBLOCK) {
                        if time.Since(lastOutputTime) > 5*time.Second {
                            logger.Printf("No PTY output for 5s at %v, recording=%v", time.Now(), recording)
                            if time.Since(lastOutputTime) > 15*time.Second {
                                logger.Printf("No PTY output for 15s, forcing stop at %v", time.Now())
                                stopRecording(true)
                                pianobarCmd.Process.Kill()
                                closeDone()
                            }
                        }
                        time.Sleep(100 * time.Millisecond)
                        continue
                    }
                    if err.Error() != "read /dev/ptmx: input/output error" {
                        logger.Printf("Error reading PTY output: %v", err)
                    }
                    closeDone()
                    return
                }
                lastOutputTime = time.Now()
                output := stripANSI(string(buf[:n]))
                if output != "" {
                    select {
                    case outputChan <- output:
                        logger.Printf("Sent %d bytes to outputChan at %v", len(output), time.Now())
                    default:
                        logger.Printf("Warning: outputChan full, dropping %d bytes at %v", len(output), time.Now())
                    }

                    songRe := regexp.MustCompile(`\|\>\s*"([^"]+)"\s*by\s*"([^"]+)"\s*on\s*"([^"]+)"`)
                    if matches := songRe.FindStringSubmatch(output); matches != nil {
                        songTitle := matches[1]
                        artist := matches[2]
                        currentSong := fmt.Sprintf("%s by %s", songTitle, artist)
                        if currentSong != lastSong {
                            logger.Printf("New song detected: %s at %v", currentSong, time.Now())
                            mu.Lock()
                            deleteFile := recording && totalDuration > 0 && remainingTime > timeThreshold
                            mu.Unlock()
                            stopRecording(deleteFile)
                            if currentStation == "" {
                                currentStation = "Unknown Station"
                            }
                            currentFileName = filepath.Join(cfg.SaveDir, currentStation, sanitizeFileName(fmt.Sprintf("%s - %s.mp3", songTitle, artist)))
                            fmt.Printf("\r\nSong detected - Starting to save: %s\n", currentFileName)
                            mu.Lock()
                            recording = true
                            mu.Unlock()
                            go saveSong(cfg, currentFileName, monitorSource)
                            lastSong = currentSong
                        } else {
                            logger.Printf("Duplicate song skipped: %s at %v", currentSong, time.Now())
                        }
                    }

                    stationRe := regexp.MustCompile(`\|\>\s*Station\s+"([^"]+)"`)
                    if matches := stationRe.FindStringSubmatch(output); matches != nil {
                        newStation := sanitizeFileName(matches[1])
                        logger.Printf("Station detected: %s", newStation)
                        if newStation != currentStation {
                            stopRecording(true)
                            currentStation = newStation
                            stationDir := filepath.Join(cfg.SaveDir, currentStation)
                            if err := os.MkdirAll(stationDir, 0755); err != nil {
                                logger.Printf("Failed to create station dir %s: %v", stationDir, err)
                            } else {
                                fmt.Printf("\r\nCreated station directory: %s\n", stationDir)
                            }
                            fmt.Printf("\r\nSwitched to station: %s\n", currentStation)
                        }
                    }

                    countdownRe := regexp.MustCompile(`#\s+-(?:(\d+):)?(\d+):(\d+)/(\d+):(\d+)`)
                    if matches := countdownRe.FindStringSubmatch(output); matches != nil {
                        remainingStr := fmt.Sprintf("%s:%s", matches[2], matches[3])
                        if matches[1] != "" {
                            remainingStr = fmt.Sprintf("%s:%s", matches[1], matches[2])
                        }
                        totalStr := fmt.Sprintf("%s:%s", matches[4], matches[5])
                        remaining, err := parseTime(remainingStr)
                        if err != nil {
                            logger.Printf("Error parsing remaining time: %v", err)
                            continue
                        }
                        total, err := parseTime(totalStr)
                        if err != nil {
                            logger.Printf("Error parsing total time: %v", err)
                            continue
                        }
                        mu.Lock()
                        remainingTime = remaining
                        totalDuration = total
                        shouldStop := remaining <= 0 && recording
                        logger.Printf("Countdown: remaining=%v, total=%v, recording=%v, shouldStop=%v", remaining, total, recording, shouldStop)
                        mu.Unlock()
                        if shouldStop {
                            fmt.Printf("\r\nSong finished, stopping capture\n")
                            stopRecording(false)
                        }
                    }

                    if strings.Contains(output, "(i) Network error") || strings.Contains(output, "Connection lost") || strings.Contains(output, "Song paused") {
                        stopRecording(true)
                        lastSong = ""
                    }
                }
            }
        }
    }()

    go func() {
        for {
            select {
            case <-done:
                return
            case <-shutdown:
                return
            case output := <-outputChan:
                fmt.Print(output)
                os.Stdout.Sync()
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

func stopRecording(deleteFile bool) {
    mu.Lock()
    defer mu.Unlock()
    logger.Printf("Entering stopRecording, ffmpegCmd=%v, recording=%v", ffmpegCmd != nil, recording)
    if ffmpegCmd != nil {
        fmt.Printf("\r\nStopping current recording\n")
        pid := ffmpegCmd.Process.Pid
        logger.Printf("Stopping FFmpeg for %s, pid=%d", currentFileName, pid)
        if err := ffmpegCmd.Process.Kill(); err != nil {
            logger.Printf("Failed to kill FFmpeg pid %d: %v", pid, err)
        } else {
            logger.Printf("Killed FFmpeg pid %d", pid)
        }
        done := make(chan error, 1)
        go func() {
            done <- ffmpegCmd.Wait()
        }()
        select {
        case err := <-done:
            if err != nil {
                logger.Printf("FFmpeg wait error for pid %d: %v", pid, err)
            } else {
                logger.Printf("FFmpeg pid %d stopped successfully", pid)
            }
        case <-time.After(2 * time.Second):
            logger.Printf("FFmpeg pid %d didn’t stop after 2s, abandoning", pid)
        }
        if deleteFile && currentFileName != "" {
            fmt.Printf("\r\nRemoving incomplete file: %s\n", currentFileName)
            os.Remove(currentFileName)
        }
        ffmpegCmd = nil
    } else {
        logger.Printf("No FFmpeg process to stop")
    }
    recording = false
    remainingTime = 0
    totalDuration = 0
}

func saveSong(cfg Config, fileName, monitorSource string) {
    logger.Printf("Starting FFmpeg for %s with 15-minute timeout", fileName)
    ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
    defer cancel()

    mu.Lock()
    ffmpegCmd = exec.CommandContext(ctx, "ffmpeg", "-f", "pulse", "-i", monitorSource, "-acodec", "mp3", "-y", fileName)
    mu.Unlock()
    if err := ffmpegCmd.Start(); err != nil {
        logger.Printf("Error starting ffmpeg for %s: %v", fileName, err)
        mu.Lock()
        ffmpegCmd = nil
        mu.Unlock()
        return
    }
    pid := ffmpegCmd.Process.Pid
    logger.Printf("FFmpeg started, pid=%d", pid)

    err := ffmpegCmd.Wait()
    mu.Lock()
    if ffmpegCmd != nil && ffmpegCmd.Process.Pid == pid {
        ffmpegCmd = nil
    }
    mu.Unlock()
    if err != nil {
        if ctx.Err() == context.DeadlineExceeded {
            logger.Printf("FFmpeg for %s timed out after 15 minutes, killed", fileName)
        } else {
            logger.Printf("Error running ffmpeg for %s: %v", fileName, err)
        }
    } else {
        logger.Printf("FFmpeg completed for %s", fileName)
    }
}

func cleanExit(pianobarCmd *exec.Cmd, code int) {
    stopRecording(true)
    pianobarCmd.Process.Kill()
    if termState != nil {
        term.Restore(int(os.Stdin.Fd()), termState)
    }
    time.Sleep(100 * time.Millisecond)
    os.Exit(code)
}

func stripANSI(s string) string {
    re := regexp.MustCompile(`\x1B\[[0-9;]*[a-zA-Z]`)
    return re.ReplaceAllString(s, "")
}

func sanitizeFileName(s string) string {
    re := regexp.MustCompile(`[<>:"/\\|?*]`)
    return re.ReplaceAllString(s, "_")
}

func parseTime(s string) (time.Duration, error) {
    parts := strings.Split(s, ":")
    if len(parts) != 2 {
        return 0, fmt.Errorf("invalid time format: %s", s)
    }
    mins, err := time.ParseDuration(parts[0] + "m")
    if err != nil {
        return 0, err
    }
    secs, err := time.ParseDuration(parts[1] + "s")
    if err != nil {
        return 0, err
    }
    return mins + secs, nil
}
