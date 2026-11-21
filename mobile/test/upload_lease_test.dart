import 'dart:io';

import 'package:cloudbox/upload_lease.dart';
import 'package:flutter_test/flutter_test.dart';
import 'package:sqflite_common_ffi/sqflite_ffi.dart';

void main() {
  late Directory directory;
  setUp(() async {
    sqfliteFfiInit();
    databaseFactory = databaseFactoryFfi;
    directory = await Directory.systemTemp.createTemp('cloudbox-lease-test-');
    await databaseFactory.setDatabasesPath(directory.path);
  });
  tearDown(() async {
    await directory.delete(recursive: true);
  });

  test('separate connections cannot run two uploads concurrently', () async {
    final first = (await UploadLease.acquire())!;
    try {
      expect(await UploadLease.acquire(), isNull);
      await first.renew();
    } finally {
      await first.release();
    }
    final next = await UploadLease.acquire();
    expect(next, isNotNull);
    await next!.release();
  });

  test(
    'expired worker cannot overwrite or release its successor lease',
    () async {
      final first = (await UploadLease.acquire())!;
      await first.db.update('lease', {'expires': 0});
      final next = (await UploadLease.acquire())!;
      await expectLater(first.renew(), throwsStateError);
      await first.release();
      expect(await UploadLease.acquire(), isNull);
      await next.renew();
      await next.release();
    },
  );
}
