#!/bin/bash

# Get the current default sink (speakers)
ORIGINAL_SINK=$(pactl get-default-sink)
if [ -z "$ORIGINAL_SINK" ]; then
    echo "Error: Could not determine original default sink" >&2
    exit 1
fi
echo "Original default sink: $ORIGINAL_SINK"

# Create a null sink for Pianobar
PIANOBAR_SINK_ID=$(pactl load-module module-null-sink sink_name=PianobarSink sink_properties=device.description=PianobarSink)
if [ -z "$PIANOBAR_SINK_ID" ]; then
    echo "Error: Failed to create PianobarSink" >&2
    exit 1
fi
echo "Created PianobarSink with module ID: $PIANOBAR_SINK_ID"

# Launch Pianobar with PULSE_SINK set to PianobarSink
PULSE_SINK=PianobarSink pianobar &
PIANOBAR_PID=$!
echo "Pianobar PID: $PIANOBAR_PID"

# Wait briefly for Pianobar to start
sleep 1

# Clone PianobarSink.monitor to the original sink for speaker playback
LOOPBACK_ID=$(pactl load-module module-loopback sink="$ORIGINAL_SINK" source=PianobarSink.monitor)
if [ -z "$LOOPBACK_ID" ]; then
    echo "Warning: Failed to create loopback to $ORIGINAL_SINK" >&2
else
    echo "Created loopback with ID: $LOOPBACK_ID to $ORIGINAL_SINK"
fi

# Wait for Pianobar to exit
wait $PIANOBAR_PID

# Cleanup
pactl unload-module "$PIANOBAR_SINK_ID"
if [ ! -z "$LOOPBACK_ID" ]; then
    pactl unload-module "$LOOPBACK_ID"
fi
echo "Cleaned up PianobarSink and loopback"
