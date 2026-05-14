#!/usr/bin/env swift
// BLE tool to configure HomeAgent relay on StackChan
// Usage: swift ble_config_homeagent.swift [get|set|reset|wifi-list|wifi-remove <index>|wifi-default <index>|wifi-clear]
//   get          - read current HomeAgent config
//   set          - write relay config (uses env vars or defaults below)
//   reset        - clear HomeAgent config
//   wifi-list    - list saved Wi-Fi profiles
//   wifi-remove  - remove a saved Wi-Fi profile by index
//   wifi-default - move a Wi-Fi profile to the front of the priority list
//   wifi-clear   - clear all saved Wi-Fi profiles

import Foundation
import CoreBluetooth

let CONFIG_SERVICE_UUID = CBUUID(string: "e2e5e5e0-1234-5678-1234-56789abcdef0")
let CONFIG_CHAR_UUID    = CBUUID(string: "e2e5e5e3-1234-5678-1234-56789abcdef0")

// Config — set via env vars
let RELAY_URL  = ProcessInfo.processInfo.environment["RELAY_URL"]  ?? ""
let RELAY_TOKEN = ProcessInfo.processInfo.environment["RELAY_TOKEN"] ?? ""
let DEVICE_ID  = ProcessInfo.processInfo.environment["DEVICE_ID"]  ?? ""

class BLEConfigurator: NSObject, CBCentralManagerDelegate, CBPeripheralDelegate {
    var central: CBCentralManager!
    var peripheral: CBPeripheral?
    var configChar: CBCharacteristic?
    let action: String
    var done = false

    init(action: String) {
        self.action = action
        super.init()
        central = CBCentralManager(delegate: self, queue: nil)
    }

    func centralManagerDidUpdateState(_ central: CBCentralManager) {
        guard central.state == .poweredOn else {
            print("BLE not available: \(central.state.rawValue)")
            return
        }
        print("Scanning for StackChan...")
        central.scanForPeripherals(withServices: [CONFIG_SERVICE_UUID], options: nil)
    }

    func centralManager(_ central: CBCentralManager, didDiscover peripheral: CBPeripheral,
                         advertisementData: [String: Any], rssi RSSI: NSNumber) {
        let name = peripheral.name ?? "unknown"
        print("Found: \(name) (\(peripheral.identifier))")
        self.peripheral = peripheral
        peripheral.delegate = self
        central.stopScan()
        central.connect(peripheral, options: nil)
    }

    func centralManager(_ central: CBCentralManager, didConnect peripheral: CBPeripheral) {
        print("Connected! Discovering services...")
        peripheral.discoverServices([CONFIG_SERVICE_UUID])
    }

    func centralManager(_ central: CBCentralManager, didFailToConnect peripheral: CBPeripheral, error: Error?) {
        print("Failed to connect: \(error?.localizedDescription ?? "unknown")")
        exit(1)
    }

    func peripheral(_ peripheral: CBPeripheral, didDiscoverServices error: Error?) {
        guard let services = peripheral.services else { return }
        for svc in services {
            if svc.uuid == CONFIG_SERVICE_UUID {
                peripheral.discoverCharacteristics([CONFIG_CHAR_UUID], for: svc)
            }
        }
    }

    func peripheral(_ peripheral: CBPeripheral, didDiscoverCharacteristicsFor service: CBService, error: Error?) {
        guard let chars = service.characteristics else { return }
        for ch in chars {
            if ch.uuid == CONFIG_CHAR_UUID {
                configChar = ch
                peripheral.setNotifyValue(true, for: ch)
                sendCommand()
            }
        }
    }

    func sendCommand() {
        guard let ch = configChar, let p = peripheral else {
            print("Config characteristic not found")
            return
        }

        var json: String
        switch action {
        case "set":
            if RELAY_URL.isEmpty || RELAY_TOKEN.isEmpty {
                print("Error: RELAY_URL and RELAY_TOKEN env vars required for 'set'")
                print("  Example: RELAY_URL=ws://example.com:8787/ws RELAY_TOKEN=xxx ./ble_config_homeagent set")
                exit(1)
            }
            json = """
            {"cmd":"setHomeAgent","data":{"enabled":true,"relayUrl":"\(RELAY_URL)","token":"\(RELAY_TOKEN)","deviceId":"\(DEVICE_ID)"}}
            """
            print("Setting HomeAgent config:")
            print("  relayUrl: \(RELAY_URL)")
            print("  token: \(String(repeating: "*", count: max(0, RELAY_TOKEN.count - 4)))\(RELAY_TOKEN.suffix(4))")
            if !DEVICE_ID.isEmpty { print("  deviceId: \(DEVICE_ID)") }
        case "reset":
            json = """
            {"cmd":"resetHomeAgent"}
            """
            print("Resetting HomeAgent config...")
        case "wifi-list":
            json = """
            {"cmd":"getWifiProfiles"}
            """
            print("Getting Wi-Fi profiles...")
        case "wifi-remove":
            let index = CommandLine.arguments.count > 2 ? CommandLine.arguments[2] : "-1"
            json = """
            {"cmd":"removeWifiProfile","data":{"index":\(index)}}
            """
            print("Removing Wi-Fi profile index \(index)...")
        case "wifi-default":
            let index = CommandLine.arguments.count > 2 ? CommandLine.arguments[2] : "-1"
            json = """
            {"cmd":"setDefaultWifiProfile","data":{"index":\(index)}}
            """
            print("Setting default Wi-Fi profile index \(index)...")
        case "wifi-clear":
            json = """
            {"cmd":"clearWifiProfiles"}
            """
            print("Clearing Wi-Fi profiles...")
        default:
            json = """
            {"cmd":"getHomeAgent"}
            """
            print("Getting HomeAgent config...")
        }

        let data = json.data(using: .utf8)!
        // BLE MTU may be small, send in chunks if needed
        let mtu = p.maximumWriteValueLength(for: .withResponse)
        var offset = 0
        while offset < data.count {
            let end = min(offset + mtu, data.count)
            let chunk = data[offset..<end]
            p.writeValue(chunk, for: ch, type: .withResponse)
            offset = end
        }
    }

    func peripheral(_ peripheral: CBPeripheral, didUpdateValueFor characteristic: CBCharacteristic, error: Error?) {
        guard let data = characteristic.value, let str = String(data: data, encoding: .utf8) else { return }
        print("Response: \(str)")
        done = true
        // Disconnect after response
        DispatchQueue.main.asyncAfter(deadline: .now() + 0.5) {
            self.central.cancelPeripheralConnection(peripheral)
            exit(0)
        }
    }

    func peripheral(_ peripheral: CBPeripheral, didWriteValueFor characteristic: CBCharacteristic, error: Error?) {
        if let error = error {
            print("Write error: \(error.localizedDescription)")
        }
    }
}

// Parse action
let action = CommandLine.arguments.count > 1 ? CommandLine.arguments[1] : "get"
let validActions = ["get", "set", "reset", "wifi-list", "wifi-remove", "wifi-default", "wifi-clear"]
guard validActions.contains(action) else {
    print("Usage: swift ble_config_homeagent.swift [get|set|reset|wifi-list|wifi-remove <index>|wifi-default <index>|wifi-clear]")
    exit(1)
}

let configurator = BLEConfigurator(action: action)

// Timeout after 30 seconds
DispatchQueue.main.asyncAfter(deadline: .now() + 30) {
    if !configurator.done {
        print("Timeout - no response received")
        exit(1)
    }
}

RunLoop.main.run()
