# busylight
DIY computer "do not disturb" indicator

This was a weekend project to see if I could take the stuff in my junk drawer and implement a simple light that would show my family members if it was safe to interrupt me without accidentally having their voices or faces appear in a meeting (this is during the work-at-home pandemic restrictions).

## Operation
The light is placed in a convenient and visible location, and displays lights to indicate my current "busy" state:

* Green: interrupt at will.
* Yellow: interrupt if important (this is shown if I happen to have anything scheduled on my Google calendars, such as a meeting or just some time blocked out to focus on a project)
* Red: in a meeting (this is shown if I'm actually connected to a video conference meeting)
* Flashing red: in a meeting, and my microphone is unmuted

## Hardware
For the light itself, I grabbed a spare Arduino Pro Micro board, a ULN2003A darlington transistor array chip, and a bunch of LEDs and resistors and threw them together.
I chose resistor values to get a suitable brightness from the LEDs (which varied by color and the voltage ratings of the LEDs; your mileage may vary) without overloading
the USB supply current limits.

The Arduino board and LEDs are powered by the USB port from the host PC, as well as using that connection for the host to send serial commands to control the lights.

## Software
On the PC, the normal mode of operation is to run the `busylightd` daemon. This monitors a set of Google calendars and reports busy/free times with the green and yellow
lights. It also responds to external signals from other processes which can inform it of other status changes.

In my case, I used [Hammerspoon](https://www.hammerspoon.org) which provides extensible automation capabilities for MacOS systems, including a very handy plugin that detects
when you join or leave a Zoom call, and tracks the state of the mute controls while in the meeting. I just configured that to send the appropriate signals to the
*busylightd* process when those statuses changed.

The upshot of that is that I can leave this running, put busy time on my calendars, join Zoom calls, and so forth, while the light indicator automatically displays
the appropriate colors.

### API Access
To get access to the Google calendar API, you'll need to register with Google and get an API key. If I were distributing this as a pre-made app, I'd include my API key
with the distribution, but as a DIY project, if you make one of these based on my design, you're essentially creating your own app anyway so it makes sense to get a
separate API key for yours.

### Documentation
There's a *busylight(1)* manual page included which explains the setup and operation of the software.

A schematic of how I wired up the hardware is also included, although it's fairly trivial, and can be adjusted to suit your needs.

# Release notes
## Version 1.5
Adds a low-priority indicator to be added in addition to the other
markers so you can say things like "I'm in a video call but it's not
so important that I can't be interrupted. Just understand I'm on camera."

Also re-added the 1.4 changes to the repo since apparently they didn't get
committed correctly before.

Also also becoming very apparent that using signals to notify the daemon
of changes in state was a bad idea (given the limited availability of
signals that are safe to co-opt for this usage), so a future version will 
likely introduce a new mechanism for that.

## Version 1.4
Allows a regular expression for the device name now, so that the daemon
can search for whatever name the OS randomly chose for the hardware device.

## Version 1.3
The daemon now closes the serial port when put in inactive state and
re-opens it when going active again. It also reloads its configuration
file at that time, making it possible to change the serial device in case
the system gave it a new dynamic device name during the time the daemon
was sleeping.

This also allows other configuration changes to be made  without restarting
the daemon (just set it to inactive state and back to active again).

Refactored some code to clean it up a little. 

## Version 1.2
The 1.2 release includes the ability to ignore long-running appointments on selected calendars, to avoid
signalling that the user is "busy" because of an all-day event on a group calendar.

This made it necessary to change the `config.json` file's list of calendars to be monitored. Users of the 1.1 version will need to 
update this configuration file after upgrading to 1.2.
