// Автозагрузка фото и видео из галереи в CloudBox («Фото/Камера/<год>»).
//
// Запускается при открытии приложения и по кнопке; загружает снятое после включения автозагрузки
// (или всю галерею, если пользователь так выбрал). Уже загруженные снимки запоминаются по id галереи,
// поэтому повторный запуск ничего не дублирует. Workmanager запускает эту же очередь в фоне.
import 'dart:io';
import 'dart:typed_data';

import 'package:photo_manager/photo_manager.dart';
import 'package:shared_preferences/shared_preferences.dart';

import 'api.dart';
import 'storage.dart';
import 'upload_lease.dart';

class AutoUploadResult {
  final int uploaded;
  final int failed;
  final String? message;
  AutoUploadResult(this.uploaded, this.failed, [this.message]);
}

class AutoUpload {
  static bool _running = false;

  static Future<bool> enabled() async {
    final p = await SharedPreferences.getInstance();
    await p.reload();
    return p.getBool(Storage.autoUploadKey) ?? false;
  }

  /// Включить. allExisting — загрузить и всю имеющуюся галерею, иначе только новые снимки.
  static Future<bool> enable({required bool allExisting}) async {
    final perm = await PhotoManager.requestPermissionExtend();
    if (!perm.hasAccess) return false;
    final p = await SharedPreferences.getInstance();
    await p.setBool(Storage.autoUploadKey, true);
    await p.setInt(Storage.autoUploadSinceKey, allExisting ? 0 : DateTime.now().millisecondsSinceEpoch);
    return true;
  }

  static Future<void> disable() async => (await SharedPreferences.getInstance()).setBool(Storage.autoUploadKey, false);

  static Future<AutoUploadResult> run(CloudBoxApi api, {void Function(String)? status, bool background = false}) async {
    if (_running) return AutoUploadResult(0, 0, 'Автозагрузка уже идёт');
    _running = true;
    UploadLease? lease;
    try {
      lease = await UploadLease.acquire();
      if (lease == null) return AutoUploadResult(0, 0, 'Автозагрузка уже идёт');
      final p = await SharedPreferences.getInstance();
      await p.reload();
      if (!(p.getBool(Storage.autoUploadKey) ?? false)) return AutoUploadResult(0, 0);
      final perm = background ? await PhotoManager.getPermissionState(requestOption: const PermissionRequestOption()) : await PhotoManager.requestPermissionExtend();
      if (!perm.hasAccess) return AutoUploadResult(0, 0, 'Нет доступа к фото — разрешите его в настройках телефона');
      final since = DateTime.fromMillisecondsSinceEpoch(p.getInt(Storage.autoUploadSinceKey) ?? 0);
      final done = (p.getStringList(Storage.autoUploadDoneKey) ?? <String>[]).toSet();
      final account = await Storage.load();
      if (account == null || CloudBoxApi.normalizeServer(account.server) != api.base || account.user != api.user) return AutoUploadResult(0, 0);
      final started = DateTime.now();
      Future<void> checkActive() async {
        await p.reload();
        final current = await Storage.load();
        if (!(p.getBool(Storage.autoUploadKey) ?? false) || current == null || current.server != account.server ||
            current.user != account.user || current.password != account.password) {
          throw const FileSystemException('Автозагрузка выключена или аккаунт изменился');
        }
        await lease!.renew();
      }

      final paths = await PhotoManager.getAssetPathList(type: RequestType.common, onlyAll: true);
      if (paths.isEmpty) return AutoUploadResult(0, 0);
      final all = paths.first;
      final total = await all.assetCountAsync;
      final todo = <AssetEntity>[];
      // галерея отсортирована от новых к старым: идём страницами, пока не дойдём до снятого раньше `since`
      outer:
      for (var page = 0; page * 100 < total; page++) {
        for (final a in await all.getAssetListPaged(page: page, size: 100)) {
          if (a.createDateTime.isBefore(since)) break outer;
          if (!done.contains(a.id)) todo.add(a);
        }
      }
      var ok = 0, failed = 0;
      for (final a in todo.reversed) { // старые первыми — в папке сохранится порядок съёмки
        // iOS refresh jobs have a short runtime; stop between files and resume next time.
        if (background && DateTime.now().difference(started) > Duration(seconds: Platform.isIOS ? 20 : 420)) break;
        await checkActive();
        status?.call('Загружаю ${ok + failed + 1} из ${todo.length}…');
        try {
          final file = await a.originFile;
          if (file == null) {
            failed++;
            continue;
          }
          final name = (await a.titleAsync).isNotEmpty ? await a.titleAsync : file.uri.pathSegments.last;
          final raf = await file.open();
          try {
            await checkActive();
            await api.upload(
              parent: null,
              name: name,
              size: await raf.length(),
              path: 'Фото/Камера/${a.createDateTime.year}',
              read: (off, len) async {
                await checkActive();
                await raf.setPosition(off);
                return Uint8List.fromList(await raf.read(len));
              },
            );
          } finally {
            await raf.close();
          }
          done.add(a.id);
          await checkActive();
          await p.setStringList(Storage.autoUploadDoneKey, done.toList());
          ok++;
        } on ApiException catch (e) {
          failed++;
          if (e.unauthorized || e.status == 507) return AutoUploadResult(ok, failed, e.message); // дальше бессмысленно
        } on FileSystemException {
          failed++;
        }
      }
      return AutoUploadResult(ok, failed);
    } finally {
      try { await lease?.release(); } finally { _running = false; }
    }
  }
}
