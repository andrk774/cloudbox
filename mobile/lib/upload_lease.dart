import 'dart:math';

import 'package:sqflite/sqflite.dart';

// SQLite coordinates foreground and Workmanager isolates, unlike an in-memory flag.
// A killed worker's lease expires so future jobs can resume.
class UploadLease {
  final Database db;
  final String owner;
  UploadLease._(this.db, this.owner);
  static Future<UploadLease?> acquire() async {
    final db = await openDatabase(
      '${await getDatabasesPath()}/autoupload-lock.db',
      version: 1,
      singleInstance: false,
      onCreate: (db, _) => db.execute(
        'CREATE TABLE lease (id INTEGER PRIMARY KEY, owner TEXT NOT NULL, expires INTEGER NOT NULL)',
      ),
    );
    final owner =
        '${DateTime.now().microsecondsSinceEpoch}-${Random.secure().nextInt(1 << 32)}';
    try {
      final acquired = await db.transaction((tx) async {
        final now = DateTime.now().millisecondsSinceEpoch;
        await tx.delete('lease', where: 'expires < ?', whereArgs: [now]);
        if ((await tx.query('lease')).isNotEmpty) return false;
        await tx.insert('lease', {
          'id': 1,
          'owner': owner,
          'expires': now + 600000,
        });
        return true;
      });
      if (acquired) return UploadLease._(db, owner);
      await db.close();
      return null;
    } catch (_) {
      await db.close();
      rethrow;
    }
  }

  Future<void> renew() async {
    final count = await db.update(
      'lease',
      {'expires': DateTime.now().millisecondsSinceEpoch + 600000},
      where: 'owner = ?',
      whereArgs: [owner],
    );
    if (count != 1) throw StateError('Загрузка передана другому заданию');
  }

  Future<void> release() async {
    try {
      await db.delete('lease', where: 'owner = ?', whereArgs: [owner]);
    } finally {
      await db.close();
    }
  }
}
