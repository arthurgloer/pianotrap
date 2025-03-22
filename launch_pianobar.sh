#!/bin/bash

ORIGINAL_SINK=$(pactl get-default-sink)
if [ -z "$ORIGINAL_SINK" ]; then
    echo "Error: Could not determine original default sink" >&2
    exit 1
fi
echo "Original default sink: $ORIGINAL_SINK"

# Check for existing PianobarSink and unload if present
EXISTING_SINK=$(pactl list sinks short | grep PianobarSink | awk '{print $1}' | head -n 1)
if [ ! -z "$EXISTING_SINK" ]; then
    EXISTING_MODULE=$(pactl list modules short | grep PianobarSink | awk '{print $1}' | head -n 1)
    if [ ! -z "$EXISTING_MODULE" ]; then
        pactl unload-module "$EXISTING_MODULE"
        echo "Unloaded existing PianobarSink module: $EXISTING_MODULE"
    fi
fi

# Create PianobarSink with correct sample rate
PIANOBAR_SINK_ID=$(pactl load-module module-null-sink sink_name=PianobarSink sink_properties=device.description=PianobarSink rate=44100 channels=2)
if [ -z "$PIANOBAR_SINK_ID" ]; then
    echo "Error: Failed to create PianobarSink" >&2
    exit 1
fi
echo "Created PianobarSink with module ID: $PIANOBAR_SINK_ID"

pactl set-sink-volume PianobarSink 65536
pactl set-sink-mute PianobarSink 0

# Loopback with matching rate and channels
LOOPBACK_ID=$(pactl load-module module-loopback sink="$ORIGINAL_SINK" source=PianobarSink.monitor rate=44100 channels=2 latency_msec=20 adjust_time=0)
if [ -z "$LOOPBACK_ID" ]; then
    echo "Warning: Failed to create loopback to $ORIGINAL_SINK" >&2
else
    echo "Created loopback with ID: $LOOPBACK_ID to $ORIGINAL_SINK"
fi

cleanup() {
    pactl unload-module "$PIANOBAR_SINK_ID" 2>/dev/null
    if [ ! -z "$LOOPBACK_ID" ]; then
        pactl unload-module "$LOOPBACK_ID" 2>/dev/null
    fi
    echo "Cleaned up PianobarSink and loopback"
    exit 0
}

trap cleanup SIGTERM SIGINT EXIT # Added EXIT to ensure cleanup on all exits

PULSE_SINK=PianobarSink pianobar
