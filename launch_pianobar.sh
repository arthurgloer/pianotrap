#!/bin/bash

ORIGINAL_SINK=$(pactl get-default-sink)
if [ -z "$ORIGINAL_SINK" ]; then
    echo "Error: Could not determine original default sink" >&2
    exit 1
fi
echo "Original default sink: $ORIGINAL_SINK"

PIANOBAR_SINK_ID=$(pactl load-module module-null-sink sink_name=PianobarSink sink_properties=device.description=PianobarSink)
if [ -z "$PIANOBAR_SINK_ID" ]; then
    echo "Error: Failed to create PianobarSink" >&2
    exit 1
fi
echo "Created PianobarSink with module ID: $PIANOBAR_SINK_ID"

pactl set-sink-volume PianobarSink 65536
pactl set-sink-mute PianobarSink 0

# Set loopback with explicit sample rate and low latency
LOOPBACK_ID=$(pactl load-module module-loopback sink="$ORIGINAL_SINK" source=PianobarSink.monitor rate=44100 latency_msec=20)
if [ -z "$LOOPBACK_ID" ]; then
    echo "Warning: Failed to create loopback to $ORIGINAL_SINK" >&2
else
    echo "Created loopback with ID: $LOOPBACK_ID to $ORIGINAL_SINK"
fi

cleanup() {
    pactl unload-module "$PIANOBAR_SINK_ID"
    if [ ! -z "$LOOPBACK_ID" ]; then
        pactl unload-module "$LOOPBACK_ID"
    fi
    echo "Cleaned up PianobarSink and loopback"
    exit 0
}

trap cleanup SIGTERM SIGINT

PULSE_SINK=PianobarSink pianobar
