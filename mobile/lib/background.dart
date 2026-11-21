import 'dart:io';

import 'package:flutter/widgets.dart';
import 'package:workmanager/workmanager.dart';

import 'api.dart';
import 'autoupload.dart';
import 'storage.dart';

@pragma('vm:entry-point')
void uploadDispatcher() {
  Workmanager().executeTask((task, data) async {
    WidgetsFlutterBinding.ensureInitialized();
    final account = await Storage.load();
    if (account == null || !await AutoUpload.enabled()) return true;
    final api = CloudBoxApi(account.server, account.user, account.password);
    try {
      final result = await AutoUpload.run(api, background: true);
      return result.failed == 0;
    } catch (_) {
      return false;
    } finally {
      api.close();
    }
  });
}

class BackgroundUpload {
  static const task = 'io.github.andrk774.cloudbox.photos';
  static bool get supported => Platform.isAndroid || Platform.isIOS;
  static Future<void> initialize() async {
    if (!supported) return;
    await Workmanager().initialize(uploadDispatcher);
    if (await AutoUpload.enabled()) await schedule();
  }

  static Future<void> schedule() async {
    if (!supported) return;
    await Workmanager().registerPeriodicTask(
      task,
      task,
      frequency: const Duration(minutes: 15),
      existingWorkPolicy: ExistingPeriodicWorkPolicy.keep,
      constraints: Constraints(
        networkType: NetworkType.unmetered,
        requiresBatteryNotLow: true,
      ),
    );
  }

  static Future<void> cancel() async {
    if (supported) await Workmanager().cancelByUniqueName(task);
  }
}
