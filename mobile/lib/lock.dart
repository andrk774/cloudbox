// Блокировка автозагрузки, общая для приложения и фоновой задачи.
//
// Фоновая задача WorkManager работает в отдельном Dart-изоляте (а иногда и процессе), поэтому флаг
// в памяти не помогает: без общей блокировки они могли бы загрузить один снимок дважды.
// Блокировка — файл, создаваемый атомарно (exclusive); зависший замок (процесс убит системой)
// считается устаревшим через [staleAfter].
import 'dart:io';

class FileLock {
  final File file;
  final Duration staleAfter;
  FileLock(String path, {this.staleAfter = const Duration(minutes: 30)}) : file = File(path);

  /// true — блокировка взята; false — её держит кто-то другой.
  Future<bool> acquire() async {
    for (var attempt = 0; attempt < 2; attempt++) {
      try {
        await file.create(exclusive: true);
        await file.writeAsString('${DateTime.now().millisecondsSinceEpoch}');
        return true;
      } on FileSystemException {
        // уже есть — проверим, не остался ли он от убитого процесса
        final stamp = int.tryParse((await _read()) ?? '');
        final age = stamp == null
            ? DateTime.now().difference(await _modified())
            : DateTime.now().difference(DateTime.fromMillisecondsSinceEpoch(stamp));
        if (age < staleAfter) return false;
        try {
          await file.delete();
        } on FileSystemException {
          return false;
        }
      }
    }
    return false;
  }

  Future<void> release() async {
    try {
      await file.delete();
    } on FileSystemException {
      // уже удалён — не страшно
    }
  }

  Future<String?> _read() async {
    try {
      return (await file.readAsString()).trim();
    } on FileSystemException {
      return null;
    }
  }

  Future<DateTime> _modified() async {
    try {
      return await file.lastModified();
    } on FileSystemException {
      return DateTime.fromMillisecondsSinceEpoch(0);
    }
  }
}
