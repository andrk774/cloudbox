// Фоновая автозагрузка на Android: периодическая задача WorkManager.
//
// Android запускает её не чаще раза в 15 минут (системный минимум) и только при выполнении условий:
// сеть (по выбору — только Wi-Fi), по выбору — только на зарядке, батарея не разряжена.
// Режимы энергосбережения могут откладывать запуск — точного расписания Android не гарантирует.
// На iOS фоновую загрузку не планируем: система не даёт надёжного способа (снимки уйдут при открытии).
import 'dart:io';
import 'dart:ui' show DartPluginRegistrant;

import 'package:shared_preferences/shared_preferences.dart';
import 'package:workmanager/workmanager.dart';

import 'api.dart';
import 'autoupload.dart';
import 'storage.dart';

const backgroundTask = 'cloudbox-autoupload';

/// Точка входа фонового изолята: её вызывает WorkManager, когда приложение закрыто.
@pragma('vm:entry-point')
void callbackDispatcher() {
  Workmanager().executeTask((task, input) async {
    DartPluginRegistrant.ensureInitialized();
    final acc = await Storage.load();
    if (acc == null) return true; // вышли из аккаунта — делать нечего
    final api = CloudBoxApi(acc.server, acc.user, acc.password);
    try {
      final r = await AutoUpload.run(api);
      // false — WorkManager повторит позже; повторяем только при сбоях сети, а не при неверном пароле или полной квоте
      return r.retry ? false : true;
    } catch (_) {
      return false;
    } finally {
      api.close();
    }
  });
}

class Background {
  static bool get supported => Platform.isAndroid;

  static Future<void> init() async {
    if (!supported) return;
    await Workmanager().initialize(callbackDispatcher);
  }

  static Future<bool> wifiOnly() async => (await SharedPreferences.getInstance()).getBool(Storage.wifiOnlyKey) ?? true;
  static Future<bool> chargingOnly() async => (await SharedPreferences.getInstance()).getBool(Storage.chargingOnlyKey) ?? false;

  static Future<void> setConditions({bool? wifiOnly, bool? chargingOnly}) async {
    final p = await SharedPreferences.getInstance();
    if (wifiOnly != null) await p.setBool(Storage.wifiOnlyKey, wifiOnly);
    if (chargingOnly != null) await p.setBool(Storage.chargingOnlyKey, chargingOnly);
    if (await AutoUpload.enabled()) await schedule();
  }

  /// Запланировать (или обновить условия) периодическую автозагрузку.
  static Future<void> schedule() async {
    if (!supported) return;
    await Workmanager().registerPeriodicTask(
      backgroundTask,
      backgroundTask,
      frequency: const Duration(minutes: 15),
      constraints: Constraints(
        networkType: await wifiOnly() ? NetworkType.unmetered : NetworkType.connected,
        requiresCharging: await chargingOnly(),
        requiresBatteryNotLow: true,
      ),
      existingWorkPolicy: ExistingPeriodicWorkPolicy.update, // новые условия заменяют старые
    );
  }

  static Future<void> cancel() async {
    if (!supported) return;
    await Workmanager().cancelByUniqueName(backgroundTask);
  }
}
