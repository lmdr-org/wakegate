# Preparing the target

The target is the computer that sleeps. Before the agent puts it to sleep, make sure the
proxy can wake it: this is the only part of the setup that depends on the hardware, and
it is worth testing by hand first.

## 1. A wired connection and a fixed address

Wake-on-LAN works with a wired network card. Most Wi-Fi cards cannot wake a computer.

Give the target a fixed address, with a DHCP reservation on your router: the proxy relays
to that address. Note the name and MAC address of the wired interface:

```console
$ ip -br link
lo        UNKNOWN  00:00:00:00:00:00
enp3s0    UP       04:d9:f5:cb:75:23
```

## 2. The firmware

In the BIOS or UEFI setup, the names vary by vendor:

- turn on the wake-up by the network card: *Wake on LAN*, *Power On By PCI-E*, *Resume
  by PCI-E Device*, *Wake on PME*;
- turn off the settings that cut the card's power when the computer is off: *ErP Ready*,
  *EuP 2013*, *Deep Sleep*, *Deep Power Off*.

Waking from suspend often works without these changes; waking from power off depends on
them. When the computer is off or asleep, the link lights of the network port should
stay on: if they go dark, the card has no power and cannot hear the magic packet.

## 3. The operating system

Check what the card accepts and what is enabled:

```console
$ sudo ethtool enp3s0 | grep Wake-on
	Supports Wake-on: pumbg
	Wake-on: d
```

`g` is the magic packet. `Wake-on: d` means disabled; it has to become `g`, in a way that
survives a reboot. Use the method that matches how the network is managed:

- **NetworkManager** (desktop distributions, Fedora):

  ```sh
  nmcli -t -f NAME,DEVICE connection show --active      # the connection's name
  sudo nmcli connection modify "Wired connection 1" 802-3-ethernet.wake-on-lan magic
  ```

- **netplan** (Ubuntu Server): add `wakeonlan: true` to the interface, then
  `sudo netplan apply`.

  ```yaml
  network:
    ethernets:
      enp3s0:
        dhcp4: true
        wakeonlan: true
  ```

- **ifupdown** (`/etc/network/interfaces`, Debian): install `ethtool` and add
  `ethernet-wol g` to the interface's stanza.

- **systemd-networkd, or none of the above**: a `.link` file. One that matches the card
  replaces `99-default.link` for it, so it repeats the naming policies, or the interface
  could be renamed:

  ```ini
  # /etc/systemd/network/50-wol.link
  [Match]
  MACAddress=04:d9:f5:cb:75:23

  [Link]
  NamePolicy=keep kernel database onboard slot path
  AlternativeNamesPolicy=database onboard slot path
  MACAddressPolicy=persistent
  WakeOnLan=magic
  ```

  On Debian and Ubuntu, the initramfs carries a copy: run `sudo update-initramfs -u`.

Reboot, and check that `ethtool` now shows `Wake-on: g`.

## 4. Test it

On the machine that will run the proxy, install wakegate (or any Wake-on-LAN tool). Then:

1. on the target: `sudo systemctl suspend`;
2. on the proxy's machine, with the target's MAC address and your network's broadcast
   address:

   ```sh
   wakegate wake -broadcast 192.168.1.255 04:d9:f5:cb:75:23
   ```

3. the target should wake within a few seconds. Try again from power off
   (`sudo systemctl poweroff`) if you plan to use it.

If nothing happens:

- check `Wake-on: g` again, and the link lights while the target sleeps;
- try `-broadcast 255.255.255.255`; check that both machines are on the same network
  (a guest Wi-Fi or another VLAN does not carry the broadcast);
- `cat /sys/class/net/enp3s0/device/power/wakeup` should say `enabled`;
- `cat /sys/power/mem_sleep` lists the suspend modes, the one in use in brackets. On some
  hardware the card wakes the computer from `deep` but not from `s2idle`; the kernel
  parameter `mem_sleep_default=deep` selects `deep` when it is listed.

## 5. Suspend, hibernate or power off

The agent's `sleep` setting picks what happens when the target is idle:

| | Wake-up | What is kept | Power drawn |
|---|---|---|---|
| `suspend` (default) | a few seconds | everything in memory: running programs, models loaded on a GPU | a few watts |
| `hibernate` | the time to read the memory back from disk | memory saved to disk; needs swap at least as large as memory, and resume configured | about the same as off |
| `poweroff` | a full boot | nothing: services start again | a watt or two, for the network card |

Check once that `sudo systemctl suspend` puts the target to sleep and that it resumes in
a usable state. For an LLM server, suspend is the natural choice: a model already
loaded answers the first request right after the wake-up.

### NVIDIA GPUs

With NVIDIA's driver, a suspend can lose the GPU's memory, and the programs using it
then fail or reload their models after the wake-up. The driver keeps it when it saves
video memory across the suspend: set `options nvidia NVreg_PreserveVideoMemoryAllocations=1`
(in `/etc/modprobe.d/`, many packages do it already) and enable `nvidia-suspend.service`,
`nvidia-resume.service` and `nvidia-hibernate.service`. Recent drivers can do it through
the kernel's suspend notifiers instead, without those services:
`options nvidia NVreg_UseKernelSuspendNotifiers=1` (tested with the 595 branch). Video
memory is copied to `/var/tmp` by default: leave room there, or point
`NVreg_TemporaryFilePath` elsewhere.

## 6. The services

- They start at boot: an enabled systemd unit, or `restart: unless-stopped` for a
  container.
- They listen on the network, not only on `127.0.0.1`. Ollama listens on localhost by
  default: `sudo systemctl edit ollama`, then add `Environment="OLLAMA_HOST=0.0.0.0"`
  under `[Service]`. llama.cpp's server takes `--host 0.0.0.0`, ComfyUI `--listen`, and
  LM Studio has *Serve on Local Network*.
- A firewall on the target lets the proxy reach the service ports, and the agent's port
  (9777) if there is an agent.

## 7. Install the agent

See the [README](../README.md#quick-start). The agent's settings are in
[configuration.md](configuration.md#agent).

## Without the agent

On a target that does not run the agent, the proxy wakes it and relays its requests, and
the target decides when to sleep. In `proxy.yaml`, leave out `target.agent`: the proxy
then considers the target awake when its first service (or `target.probe`) accepts a
connection.

- **Windows.** In Device Manager, open the network adapter: under *Power Management*,
  tick *Allow this device to wake the computer* and *Only allow a magic packet to wake
  the computer*; under *Advanced*, enable *Wake on Magic Packet*. To wake it from shut
  down, turn off *Fast startup* (Control Panel, Power Options, *Choose what the power
  buttons do*). Windows goes to sleep after the time set in *Settings, System, Power*,
  counted from the last keyboard or mouse input, not from the last network request: a
  long request can be cut off unless the server application keeps the computer awake
  while it works. `powercfg /requests` lists what does.
- **macOS.** In *System Settings*, *Energy* (or *Battery, Options* on a laptop), turn on
  *Wake for network access*.
