/*
SPDX-FileCopyrightText: 2026 M5Stack Technology CO LTD
SPDX-License-Identifier: MIT
*/

import 'dart:convert';

import 'package:flutter/cupertino.dart';
import 'package:stack_chan/app_state.dart';
import 'package:stack_chan/model/home_agent_config.dart';
import 'package:stack_chan/util/blue_util.dart';
import 'package:stack_chan/view/app.dart';

class HomeAgentConfigPage extends StatefulWidget {
  const HomeAgentConfigPage({super.key});

  @override
  State<HomeAgentConfigPage> createState() => _HomeAgentConfigPageState();
}

class _HomeAgentConfigPageState extends State<HomeAgentConfigPage> {
  final TextEditingController _relayController = TextEditingController();
  final TextEditingController _deviceIdController = TextEditingController();
  final TextEditingController _tokenController = TextEditingController();
  bool _enabled = true;
  bool _saving = false;

  @override
  void initState() {
    super.initState();
    _deviceIdController.text = AppState.shared.deviceMac;
    BlueUtil.shared.wifiSetCharacteristicCall = _handleBleNotify;
  }

  @override
  void dispose() {
    BlueUtil.shared.wifiSetCharacteristicCall = null;
    _relayController.dispose();
    _deviceIdController.dispose();
    _tokenController.dispose();
    super.dispose();
  }

  @override
  Widget build(BuildContext context) {
    return CupertinoPageScaffold(
      backgroundColor: CupertinoColors.systemGroupedBackground.resolveFrom(
        context,
      ),
      navigationBar: CupertinoNavigationBar(
        middle: const Text('HomeAgent'),
        trailing: CupertinoButton(
          padding: EdgeInsets.zero,
          onPressed: _saving ? null : _save,
          child: _saving
              ? const CupertinoActivityIndicator()
              : const Text('Save'),
        ),
      ),
      child: SafeArea(
        child: ListView(
          children: [
            CupertinoListSection.insetGrouped(
              header: const Text('Relay'),
              children: [
                CupertinoListTile(
                  title: CupertinoTextField(
                    controller: _relayController,
                    placeholder: 'wss://relay.example.com/ws',
                    textInputAction: TextInputAction.next,
                    autocorrect: false,
                  ),
                ),
                CupertinoListTile(
                  title: CupertinoTextField(
                    controller: _deviceIdController,
                    placeholder: 'Device ID',
                    textInputAction: TextInputAction.next,
                    autocorrect: false,
                  ),
                ),
                CupertinoListTile(
                  title: CupertinoTextField(
                    controller: _tokenController,
                    placeholder: 'Relay token',
                    obscureText: true,
                    textInputAction: TextInputAction.done,
                    autocorrect: false,
                  ),
                ),
              ],
            ),
            CupertinoListSection.insetGrouped(
              children: [
                CupertinoListTile(
                  title: const Text('Enable HomeAgent relay'),
                  trailing: CupertinoSwitch(
                    value: _enabled,
                    onChanged: (value) => setState(() => _enabled = value),
                  ),
                ),
                CupertinoListTile(
                  title: const Text('Read current config'),
                  onTap: _readConfig,
                  trailing: const Icon(CupertinoIcons.chevron_right),
                ),
                CupertinoListTile(
                  title: const Text('Reset HomeAgent config'),
                  onTap: _reset,
                  trailing: const Icon(CupertinoIcons.xmark_circle),
                ),
              ],
            ),
            Padding(
              padding: const EdgeInsets.symmetric(horizontal: 32, vertical: 12),
              child: Text(
                'Use the same device ID and token on the VPS relay and the home computer bridge.',
                style: TextStyle(
                  color: CupertinoColors.secondaryLabel.resolveFrom(context),
                ),
              ),
            ),
          ],
        ),
      ),
    );
  }

  void _handleBleNotify(List<int> data) {
    try {
      final jsonString = utf8.decode(data);
      final map = jsonDecode(jsonString) as Map<String, dynamic>;
      if (map['cmd'] != 'notifyHomeAgent') {
        return;
      }
      final payload = map['data'] as Map<String, dynamic>?;
      if (payload == null) {
        return;
      }
      setState(() {
        _enabled = payload['enabled'] == true;
        _relayController.text = payload['relayUrl']?.toString() ?? '';
        _deviceIdController.text = payload['deviceId']?.toString() ?? '';
        if (payload['hasToken'] != true) {
          _tokenController.text = '';
        }
      });
      AppState.shared.showToast(
        payload['state']?.toString() ?? 'HomeAgent config updated.',
      );
    } catch (_) {
      AppState.shared.showToast('Failed to parse HomeAgent config.');
    }
  }

  Future<void> _save() async {
    if (_relayController.text.trim().isEmpty) {
      AppState.shared.showToast('Please enter the relay URL.');
      return;
    }
    setState(() => _saving = true);
    final command = HomeAgentConfigCommand(
      cmd: 'setHomeAgent',
      data: HomeAgentConfig(
        enabled: _enabled,
        relayUrl: _relayController.text.trim(),
        deviceId: _deviceIdController.text.trim(),
        token: _tokenController.text,
      ),
    );
    final ok = await BlueUtil.shared.sendWifiSetData(command.toJson());
    setState(() => _saving = false);
    if (!ok) {
      App.showDialog('Bluetooth disconnected. Please reconnect StackChan.');
      return;
    }
    AppState.shared.showToast('HomeAgent config sent.');
  }

  Future<void> _readConfig() async {
    final ok = await BlueUtil.shared.sendWifiSetData(
      const HomeAgentConfigCommand(cmd: 'getHomeAgent').toJson(),
    );
    if (!ok) {
      App.showDialog('Bluetooth disconnected. Please reconnect StackChan.');
    }
  }

  Future<void> _reset() async {
    final ok = await BlueUtil.shared.sendWifiSetData(
      const HomeAgentConfigCommand(cmd: 'resetHomeAgent').toJson(),
    );
    if (!ok) {
      App.showDialog('Bluetooth disconnected. Please reconnect StackChan.');
      return;
    }
    AppState.shared.showToast('HomeAgent config reset.');
  }
}
