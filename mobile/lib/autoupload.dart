// Автозагрузка фото и видео из галереи в CloudBox («Фото/Камера/<год>»).
//
// Запускается при открытии приложения, по кнопке и (на Android) периодически в фоне — background.dart.
// Загружает снятое после включения автозагрузки (или всю галерею, если пользователь так выбрал).
// Уже загруженные снимки запоминаются по id галереи; приложение и фоновая задача делят одну
// файловую блокировку, поэтому один снимок не уйдёт дважды.
import 'dart:io';
import 'dart:typed_data';

import 'package:path_provider/path_provider.dart';
import 'package:photo_manager/photo_manager.dart';
import 'package:shared_preferences/shared_preferences.dart';

import 'api.dart';
import 'background.dart';
import 'lock.dart';
import 'storage.dart';

class AutoUploadResult {
  final int uploaded;
  final int failed;
  final String? message;
  final bool retry; // фоновой задаче стоит повторить позже (сбой сети), а не ждать следующего окна
  AutoUploadResult(this.uploaded, this.failed, [this.message, this.retry = false]);
}

class AutoUpload {
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
    await Background.schedule();
    return true;
  }

  static Future<void> disable() async {
    await (await SharedPreferences.getInstance()).setBool(Storage.autoUploadKey, false);
    await Background.cancel();
  }

  static Future<FileLock> _lock() async => FileLock('${(await getApplicationSupportDirectory()).path}/autoupload.lock');

  static Future<AutoUploadResult> run(CloudBoxApi api, {void Function(String)? status}) async {
    final lock = await _lock();
    if (!await lock.acquire()) return AutoUploadResult(0, 0, 'Автозагрузка уже идёт');
    try {
      final p = await SharedPreferences.getInstance();
      await p.reload(); // список загруженного мог пополнить другой изолят (фоновая задача)
      if (!(p.getBool(Storage.autoUploadKey) ?? false)) return AutoUploadResult(0, 0);
      final perm = await PhotoManager.requestPermissionExtend();
      if (!perm.hasAccess) return AutoUploadResult(0, 0, 'Нет доступа к фото — разрешите его в настройках телефона');
      final since = DateTime.fromMillisecondsSinceEpoch(p.getInt(Storage.autoUploadSinceKey) ?? 0);
      final done = (p.getStringList(Storage.autoUploadDoneKey) ?? <String>[]).toSet();

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
      var networkError = false;
      for (final a in todo.reversed) { // старые первыми — в папке сохранится порядок съёмки
        status?.call('Загружаю ${ok + failed + 1} из ${todo.length}…');
        try {
          final file = await a.originFile;
          if (file == null) {
            failed++;
            continue;
          }
          final title = await a.titleAsync;
          final name = title.isNotEmpty ? title : file.uri.pathSegments.last;
          final raf = await file.open();
          try {
            await api.upload(
              parent: null,
              name: name,
              size: await raf.length(),
              path: 'Фото/Камера/${a.createDateTime.year}',
              read: (off, len) async {
                await raf.setPosition(off);
                return Uint8List.fromList(await raf.read(len));
              },
            );
          } finally {
            await raf.close();
          }
          done.add(a.id);
          await p.setStringList(Storage.autoUploadDoneKey, done.toList());
          ok++;
        } on ApiException catch (e) {
          failed++;
          if (e.unauthorized || e.status == 507) return AutoUploadResult(ok, failed, e.message); // дальше бессмысленно
          if (e.status == 0) { // нет связи — остальное тоже не уйдёт, попробуем позже
            networkError = true;
            break;
          }
        } on FileSystemException {
          failed++;
        }
      }
      await p.setInt(Storage.autoUploadLastRunKey, DateTime.now().millisecondsSinceEpoch);
      return AutoUploadResult(ok, failed, networkError ? 'Нет связи с сервером — продолжу позже' : null, networkError);
    } finally {
      await lock.release();
    }
  }
}
