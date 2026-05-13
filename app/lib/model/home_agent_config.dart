/*
SPDX-FileCopyrightText: 2026 M5Stack Technology CO LTD
SPDX-License-Identifier: MIT
*/

import 'dart:convert';

class HomeAgentConfig {
  final bool enabled;
  final String relayUrl;
  final String deviceId;
  final String token;

  const HomeAgentConfig({
    required this.enabled,
    required this.relayUrl,
    required this.deviceId,
    required this.token,
  });

  Map<String, dynamic> toMap() {
    return {
      'enabled': enabled,
      'relayUrl': relayUrl,
      'deviceId': deviceId,
      'token': token,
    };
  }
}

class HomeAgentConfigCommand {
  final String cmd;
  final HomeAgentConfig? data;

  const HomeAgentConfigCommand({required this.cmd, this.data});

  String toJson() {
    return const JsonEncoder.withIndent(
      '  ',
    ).convert({'cmd': cmd, if (data != null) 'data': data!.toMap()});
  }
}
